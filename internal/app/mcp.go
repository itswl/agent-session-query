package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// MCP server (stdio transport, JSON-RPC 2.0, one JSON value per line).
//
// This tool's users are people with six agent CLIs installed at once, which makes letting
// an agent query its own history a natural fit: Claude Code can search for the same
// problem you solved with Codex last week. The HTTP API was already callable by an agent;
// what MCP adds is discoverability and a parameter schema.
//
// Hard rule: stdout carries JSON-RPC and nothing else. Logs and warnings all go to stderr,
// or they corrupt the protocol stream.
const (
	mcpProtocolVersion = "2025-06-18"
	mcpDefaultLimit    = 20
	maxMCPBodyBytes    = 1 << 20 // 1 MB request body cap; these are query parameters, nothing more
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type mcpServer struct {
	api      *SessionQueryAPI
	sources  []SessionSource
	maxLimit int
}

// runMCP drives the MCP loop over stdio and returns the process exit code.
func runMCP(s *mcpServer, in io.Reader, out io.Writer) int {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)

	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			// Bad input: say so and stop. Reading on would just hit the same bad byte forever
			fmt.Fprintf(os.Stderr, "[ERROR] failed to parse MCP input: %v\n", err)
			return 1
		}

		// stdio has no per-request lifetime to cancel against
		result, rpcErr := s.dispatch(context.Background(), req.Method, req.Params)
		if len(req.ID) == 0 {
			continue // a notification (no id) needs no reply
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] failed to write MCP output: %v\n", err)
			return 1
		}
	}
}

func (s *mcpServer) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "agent-session-query", "version": buildVersion},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case "tools/call":
		return s.callTool(ctx, params)
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "unknown method: " + method}
	}
}

func (s *mcpServer) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &rpcError{Code: -32602, Message: "bad params: " + err.Error()}
	}

	payload, err := s.runTool(ctx, call.Name, call.Arguments)
	if err != nil {
		// Per the MCP convention, tool-level failures travel as isError rather than a protocol
		// error, so the model can read what went wrong and retry with different arguments
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			"isError": true,
		}, nil
	}
	body, jsonErr := json.MarshalIndent(payload, "", "  ")
	if jsonErr != nil {
		return nil, &rpcError{Code: -32603, Message: jsonErr.Error()}
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(body)}},
	}, nil
}

func (s *mcpServer) runTool(ctx context.Context, name string, args map[string]any) (any, error) {
	switch name {
	case "search_sessions":
		query := strings.TrimSpace(argString(args, "query"))
		if query == "" {
			return nil, errors.New("missing argument: query")
		}
		q := searchQuery{
			needle:     query,
			lowered:    appendLowerASCII(nil, []byte(query)),
			limit:      argInt(args, "limit", mcpDefaultLimit, s.maxLimit),
			perSession: argInt(args, "per_session", defaultSearchPerSession, s.maxLimit),
		}
		if raw := strings.TrimSpace(argString(args, "since")); raw != "" {
			since, err := parseSince(raw)
			if err != nil {
				return nil, err
			}
			q.since = since
		}
		found := s.api.search(ctx, q)
		return map[string]any{
			"results": found.results, "matched": found.matched,
			"scanned": found.scanned, "truncated": found.matched > len(found.results),
		}, nil

	case "list_sessions":
		sessions, _ := s.api.listSessions()
		if want := strings.TrimSpace(argString(args, "source")); want != "" {
			filtered := sessions[:0:0]
			for _, item := range sessions {
				if toStr(item["source"]) == want {
					filtered = append(filtered, item)
				}
			}
			sessions = filtered
		}
		if project := strings.TrimSpace(argString(args, "project")); project != "" {
			filtered := sessions[:0:0]
			for _, item := range sessions {
				if strings.Contains(toStr(item["project"]), project) {
					filtered = append(filtered, item)
				}
			}
			sessions = filtered
		}
		limit := argInt(args, "limit", mcpDefaultLimit, s.maxLimit)
		total := len(sessions)
		if len(sessions) > limit {
			sessions = sessions[:limit]
		}
		return map[string]any{"sessions": sessions, "total": total, "returned": len(sessions)}, nil

	case "list_projects":
		projects, ungrouped := s.api.listProjects()
		return map[string]any{"projects": projects, "total": len(projects), "ungrouped": ungrouped}, nil

	case "get_session":
		pattern := strings.TrimSpace(argString(args, "pattern"))
		if pattern == "" {
			return nil, errors.New("missing argument: pattern")
		}
		session, ok := s.api.getSession(pattern)
		if !ok {
			return nil, fmt.Errorf("no session matches %q", pattern)
		}
		final, _ := s.api.getFinalMessage(pattern)
		return map[string]any{"session": session, "final": final}, nil

	case "get_messages":
		pattern := strings.TrimSpace(argString(args, "pattern"))
		if pattern == "" {
			return nil, errors.New("missing argument: pattern")
		}
		q := messageQuery{
			limit:   argInt(args, "limit", 50, s.maxLimit),
			fromEnd: strings.EqualFold(argString(args, "order"), "desc"),
		}
		messages, ok := s.api.getMessages(pattern, q)
		if !ok {
			return nil, fmt.Errorf("no session matches %q", pattern)
		}
		return map[string]any{"messages": messages, "total": len(messages), "order": orderName(q.fromEnd)}, nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	return toStr(args[key])
}

func argInt(args map[string]any, key string, def, max int) int {
	n := def
	if args != nil {
		if v, ok := toFloat(args[key]); ok && v >= 0 {
			n = int(v)
		}
	}
	if max > 0 && n > max {
		n = max
	}
	return n
}

func strSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intSchema(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func mcpTools() []map[string]any {
	return []map[string]any{
		{
			"name": "search_sessions",
			"description": "Full-text search across the session history of every agent CLI on this " +
				"machine (Claude Code, Codex, Gemini CLI, Pi, Hermes, OpenClaw). Answers " +
				"\"which session did I deal with X in?\". Case-insensitive.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":       strSchema("text to search for"),
					"limit":       intSchema("how many sessions to return at most, default 20"),
					"per_session": intSchema("how many hits per session at most, default 3"),
					"since":       strSchema("only search sessions updated after this, e.g. 30d / 12h / 2026-09-01; narrows the scan when history is large"),
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "list_sessions",
			"description": "List sessions newest first, optionally filtered by source or project (cwd).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"source":  strSchema("restrict to one source: " + strings.Join(knownModes, " / ")),
					"project": strSchema("filter by project path (cwd), substring match"),
					"limit":   intSchema("how many to return at most, default 20"),
				},
			},
		},
		{
			"name":        "list_projects",
			"description": "Group sessions by project (cwd) to see which agents were used on a given repository, and how many sessions each has.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "get_session",
			"description": "Fetch one session's metadata and final result. pattern may be a full sessionId, a fragment of one, or a fragment of the file path.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"pattern": strSchema("a sessionId, a fragment of one, or a file path fragment")},
				"required":   []string{"pattern"},
			},
		},
		{
			"name":        "get_messages",
			"description": "Fetch a session's messages. The interesting part of a long session is usually its end, so use order=desc for the latest N.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": strSchema("a sessionId, a fragment of one, or a file path fragment"),
					"limit":   intSchema("how many to return at most, default 50"),
					"order":   strSchema("asc for the earliest N (default), desc for the latest N"),
				},
				"required": []string{"pattern"},
			},
		},
	}
}

// mcpStartupBanner: in MCP mode the startup output can only go to stderr
func mcpStartupBanner(mode string, sources []SessionSource) {
	fmt.Fprintf(os.Stderr, "MCP server (stdio) ready · mode %s · sources:", mode)
	for _, source := range sources {
		fmt.Fprintf(os.Stderr, " %s", source.Mode())
	}
	fmt.Fprintf(os.Stderr, "\nstarted at %s\n", time.Now().Format(time.RFC3339))
}

// ---------------------------------------------------------------------------
// Streamable HTTP transport (POST /mcp)
// ---------------------------------------------------------------------------

// MCP's Streamable HTTP: one endpoint that accepts JSON-RPC.
//
// This implementation is stateless — every request carries its full context and the server
// keeps no session, so it issues no Mcp-Session-Id (which the spec permits). There are no
// server-initiated messages either, so GET returns 405 as the spec requires rather than
// holding an empty SSE stream open. The 2025-06-18 spec dropped JSON-RPC batching, so only
// a single message is accepted.
//
// One security requirement is spelled out in the spec: Origin must be validated, or any
// web page could POST to the local MCP endpoint (DNS rebinding). Like /sessions, this
// endpoint also requires authentication.
func (s *apiServer) handleMCPPost(w http.ResponseWriter, r *http.Request) int {
	if !s.allowedMCPOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "Origin not allowed"})
		return http.StatusForbidden
	}
	if !s.checkAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return http.StatusUnauthorized
	}

	var req rpcRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMCPBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
		})
		return http.StatusBadRequest
	}

	result, rpcErr := s.mcp.dispatch(r.Context(), req.Method, req.Params)
	if len(req.ID) == 0 {
		// A notification has no id; per the spec there is no reply, only an acknowledgement
		w.WriteHeader(http.StatusAccepted)
		return http.StatusAccepted
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		resp.Result = result
	}
	// A JSON-RPC-level error is still a successful HTTP exchange, so the status stays 200
	writeJSON(w, http.StatusOK, resp)
	return http.StatusOK
}

// allowedMCPOrigin guards against DNS rebinding. A request carrying Origin came from a
// browser; native MCP clients never send that header. So "no Origin" passes, while an
// Origin that is present must match --cors-origin or the request is refused.
func (s *apiServer) allowedMCPOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return s.corsOrigin == "*" || (s.corsOrigin != "" && s.corsOrigin == origin)
}
