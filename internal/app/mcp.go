package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
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
		if raw := strings.TrimSpace(argString(args, "until")); raw != "" {
			until, err := parseSince(raw)
			if err != nil {
				return nil, fmt.Errorf("bad until value: %w", err)
			}
			q.until = until
		}
		q.pattern = strings.TrimSpace(argString(args, "pattern"))
		role := strings.TrimSpace(argString(args, "role"))
		if role != "" && role != "user" && role != "assistant" {
			return nil, fmt.Errorf("role must be user or assistant, got %q", role)
		}
		q.role = role
		found := s.api.search(ctx, q)
		offset := decodeCursor(argString(args, "cursor"))
		if offset > len(found.results) {
			offset = len(found.results)
		}
		results := found.results[offset:]
		if len(results) > q.limit {
			results = results[:q.limit]
		}
		out := map[string]any{
			"results": results, "matched": found.matched,
			"scanned": found.scanned, "truncated": found.matched > len(found.results),
		}
		if offset+len(results) < len(found.results) {
			out["nextCursor"] = encodeCursor(offset + len(results))
		}
		return out, nil

	case "list_sessions":
		sessions, _ := s.api.listSessions()
		if want, err := wantedSource(args); err != nil {
			return nil, err
		} else if want != "" {
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
		// since/until bound the update time; a session with no parseable time is left out
		// of a bounded query rather than guessed into either side
		window, err := parseTimeWindow(args)
		if err != nil {
			return nil, err
		}
		if !window.since.IsZero() || !window.until.IsZero() {
			filtered := sessions[:0:0]
			for _, item := range sessions {
				at, ok := parseTimestamp(toStr(item["updatedAt"]))
				if !ok {
					continue
				}
				if !window.since.IsZero() && at.Before(window.since) {
					continue
				}
				if !window.until.IsZero() && at.After(window.until) {
					continue
				}
				filtered = append(filtered, item)
			}
			sessions = filtered
		}
		total := len(sessions)
		limit := argInt(args, "limit", mcpDefaultLimit, s.maxLimit)
		offset := decodeCursor(argString(args, "cursor"))
		if offset > total {
			offset = total
		}
		sessions = sessions[offset:]
		if len(sessions) > limit {
			sessions = sessions[:limit]
		}
		out := map[string]any{"sessions": sessions, "total": total, "returned": len(sessions)}
		if offset+len(sessions) < total {
			out["nextCursor"] = encodeCursor(offset + len(sessions))
		}
		return out, nil

	case "list_projects":
		projects, ungrouped := s.api.listProjects()
		return map[string]any{"projects": projects, "total": len(projects), "ungrouped": ungrouped}, nil

	case "recent_project_activity":
		project := strings.TrimSpace(argString(args, "project"))
		if project == "" {
			return nil, errors.New("missing argument: project")
		}
		sessions, _ := s.api.listSessions()
		limit := argInt(args, "limit", mcpDefaultLimit, s.maxLimit)
		activity := make([]map[string]any, 0)
		for _, item := range sessions {
			if !strings.Contains(toStr(item["project"]), project) {
				continue
			}
			activity = append(activity, item)
			if len(activity) >= limit {
				break
			}
		}
		return map[string]any{"project": project, "activity": activity, "returned": len(activity)}, nil

	case "find_decisions", "find_similar_question":
		query := strings.TrimSpace(argString(args, "query"))
		if query == "" {
			return nil, errors.New("missing argument: query")
		}
		limit := argInt(args, "limit", mcpDefaultLimit, s.maxLimit)
		// Search each term independently and merge by session. This gives an Agent useful
		// recall without pretending that a lexical match is a generated memory or decision.
		terms := strings.FieldsFunc(query, func(r rune) bool { return r == ',' || r == '|' })
		merged := map[string]map[string]any{}
		order := []string{}
		for _, raw := range terms {
			term := strings.TrimSpace(raw)
			if term == "" {
				continue
			}
			q := searchQuery{needle: term, lowered: appendLowerASCII(nil, []byte(term)), limit: limit, perSession: 5, role: "assistant"}
			found := s.api.search(ctx, q)
			for _, item := range found.results {
				key := toStr(item["source"]) + "\x00" + toStr(item["sessionId"])
				if _, exists := merged[key]; !exists {
					merged[key] = item
					order = append(order, key)
				} else {
					merged[key]["matchCount"] = toFloatDefault(merged[key]["matchCount"], 0) + toFloatDefault(item["matchCount"], 0)
				}
			}
		}
		results := make([]map[string]any, 0)
		for _, key := range order {
			if len(results) >= limit {
				break
			}
			results = append(results, merged[key])
		}
		return map[string]any{"query": query, "results": results, "matched": len(order), "note": "lexical cross-session matches; inspect excerpts before treating them as authoritative memory"}, nil

	case "get_session":
		pattern := strings.TrimSpace(argString(args, "pattern"))
		if pattern == "" {
			return nil, errors.New("missing argument: pattern")
		}
		sourceWanted, err := wantedSource(args)
		if err != nil {
			return nil, err
		}
		session, ok := s.api.getSession(pattern, sourceWanted)
		if !ok {
			return nil, fmt.Errorf("no session matches %q", pattern)
		}
		final, _ := s.api.getFinalMessage(pattern, sourceWanted)
		return map[string]any{"session": session, "final": final}, nil

	case "get_messages":
		pattern := strings.TrimSpace(argString(args, "pattern"))
		if pattern == "" {
			return nil, errors.New("missing argument: pattern")
		}
		limit := argInt(args, "limit", 50, s.maxLimit)
		sourceWanted, err := wantedSource(args)
		if err != nil {
			return nil, err
		}
		offset := decodeCursor(argString(args, "cursor"))
		q := messageQuery{
			limit:   limit,
			fromEnd: strings.EqualFold(argString(args, "order"), "desc"),
		}
		// Anchoring is how a search hit in the middle of a long session is reachable:
		// without it the window only ever comes from one end
		if raw := strings.TrimSpace(argString(args, "at")); raw != "" {
			at, err := parseAt(raw)
			if err != nil {
				return nil, err
			}
			q.at = at
		}
		// A role filter applies before the limit, so limit stays "N of this role" rather
		// than "N of everything, then whatever survived". That needs the whole slice, so
		// the fetch is widened and cut back afterwards.
		role := strings.TrimSpace(argString(args, "role"))
		if role != "" && role != "user" && role != "assistant" {
			return nil, fmt.Errorf("role must be user or assistant, got %q", role)
		}
		// Without a role the source layer only hands back q.limit messages, so every
		// page fetches one more than it shows: the extra message is the only way to
		// know another page follows. (The earlier version decided "more pages?" from
		// len(messages) after the source had already truncated to limit — always
		// false, and nextCursor never appeared.)
		if role == "" {
			q.limit = min(offset+limit+1, s.maxLimit)
		}
		messages, ok := s.api.getMessages(pattern, sourceWanted, q)
		if !ok {
			return nil, fmt.Errorf("no session matches %q", pattern)
		}
		if role != "" {
			filtered := messages[:0:0]
			for _, m := range messages {
				if toStr(m["role"]) == role {
					filtered = append(filtered, m)
				}
			}
			messages = filtered
		}
		// total is what the fetch produced, not the session's true message count: the
		// source caps at q.limit. It still tells the client whether this page is full
		// (more may follow) and stays honest about what was actually read.
		total := len(messages)
		messages = pageMessages(messages, offset, limit, q.fromEnd)
		out := map[string]any{"messages": messages, "total": total, "order": orderName(q.fromEnd)}
		if role != "" {
			out["role"] = role
		}
		more := len(messages) == limit && offset+limit < total
		if more {
			out["nextCursor"] = encodeCursor(offset + limit)
		}
		return out, nil

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func toFloatDefault(value any, fallback float64) float64 {
	if n, ok := toFloat(value); ok {
		return n
	}
	return fallback
}

// pageMessages slices one page out of the fetched window. With fromEnd the fetch holds
// the newest q.limit messages in chronological order, so page k counts back from the end;
// ascending pages count forward from the start.
func pageMessages(fetched []map[string]any, offset, limit int, fromEnd bool) []map[string]any {
	if fromEnd {
		end := len(fetched) - offset
		if end < 0 {
			end = 0
		}
		start := end - limit
		if start < 0 {
			start = 0
		}
		return fetched[start:end]
	}
	if offset > len(fetched) {
		offset = len(fetched)
	}
	out := fetched[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// wantedSource reads the optional source argument and rejects values that name no
// configured source — a typo should fail loudly, not silently match nothing.
func wantedSource(args map[string]any) (string, error) {
	want := strings.TrimSpace(argString(args, "source"))
	if want == "" {
		return "", nil
	}
	for _, source := range knownModes {
		if source == want {
			return want, nil
		}
	}
	return "", fmt.Errorf("unknown source %q (choose from: %s)", want, strings.Join(knownModes, " / "))
}

// parseTimeWindow reads the since/until pair off a tool call. Both use the same relative
// grammar (30d means "30 days ago" either side of the comparison).
func parseTimeWindow(args map[string]any) (struct {
	since, until time.Time
}, error) {
	var w struct {
		since, until time.Time
	}
	if raw := strings.TrimSpace(argString(args, "since")); raw != "" {
		t, err := parseSince(raw)
		if err != nil {
			return w, err
		}
		w.since = t
	}
	if raw := strings.TrimSpace(argString(args, "until")); raw != "" {
		t, err := parseSince(raw)
		if err != nil {
			return w, fmt.Errorf("bad until value: %w", err)
		}
		w.until = t
	}
	return w, nil
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

// readOnlyAnnotations marks every tool here: they only read session data, never modify
// it, touch nothing outside this machine, and repeating one returns the same answer.
// Clients use readOnlyHint to skip call confirmations, so it must stay truthful — the
// day a tool writes anything, its annotation has to go.
func readOnlyAnnotations(title string) map[string]any {
	return map[string]any{
		"title":           title,
		"readOnlyHint":    true,
		"idempotentHint":  true,
		"openWorldHint":   false,
		"destructiveHint": false,
	}
}

// Cursors are opaque offsets: encodeCursor/decodeCursor keep the wire format
// base64 so a client never mistakes one for a plain number. A cursor is only
// meaningful for the same tool and arguments it was issued with.
func encodeCursor(offset int) string {
	return base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeCursor(raw string) int {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(string(decoded))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// cursorSchema is the same description on every paginated tool
func cursorSchema() map[string]any {
	return strSchema("continue from a previous page: pass back the nextCursor the last call returned")
}

func intSchema(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func mcpTools() []map[string]any {
	return []map[string]any{
		{
			"name": "search_sessions",
			// The source list is derived, not written out: it had already drifted once,
			// still naming six sources after the seventh was added.
			"description": "Full-text search across the session history of every agent CLI on this " +
				"machine (" + strings.Join(knownModes, ", ") + "). Answers " +
				"\"which session did I deal with X in?\". Case-insensitive.",
			"annotations": readOnlyAnnotations("Search sessions"),
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":       strSchema("text to search for"),
					"limit":       intSchema("how many sessions to return at most, default 20"),
					"per_session": intSchema("how many hits per session at most, default 3"),
					"since":       strSchema("only search sessions updated after this, e.g. 30d / 12h / 2026-09-01; narrows the scan when history is large"),
					"until":       strSchema("only search sessions updated before this, e.g. 7d (a week ago) / 2026-09-01"),
					"pattern":     strSchema("limit the search to one session: a sessionId, a fragment of one, or a file path fragment — the way to ask \"where in this session did we discuss X\" without paging through it"),
					"role":        strSchema("keep only hits from this role: user or assistant"),
					"cursor":      cursorSchema(),
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "list_sessions",
			"description": "List sessions newest first, optionally filtered by source, project (cwd) or update time.",
			"annotations": readOnlyAnnotations("List sessions"),
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"source":  strSchema("restrict to one source: " + strings.Join(knownModes, " / ")),
					"project": strSchema("filter by project path (cwd), substring match"),
					"since":   strSchema("only sessions updated after this, e.g. 30d / 12h / 2026-09-01"),
					"until":   strSchema("only sessions updated before this, e.g. 7d (a week ago) / 2026-09-01"),
					"limit":   intSchema("how many to return at most, default 20"),
					"cursor":  cursorSchema(),
				},
			},
		},
		{
			"name":        "list_projects",
			"description": "Group sessions by project (cwd) to see which agents were used on a given repository, and how many sessions each has.",
			"annotations": readOnlyAnnotations("List projects"),
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "recent_project_activity",
			"description": "Read-only project timeline: list the most recently updated sessions for a project across agent sources.",
			"annotations": readOnlyAnnotations("Recent project activity"),
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"project": strSchema("project path or substring of cwd"), "limit": intSchema("maximum sessions, default 20"),
			}, "required": []string{"project"}},
		},
		{
			"name":        "find_decisions",
			"description": "Find likely decision and conclusion excerpts across sessions. This is lexical, not an AI-generated summary.",
			"annotations": readOnlyAnnotations("Find decisions"),
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"query": strSchema("terms to search, separated by comma or |"), "limit": intSchema("maximum sessions, default 20"),
			}},
		},
		{
			"name":        "find_similar_question",
			"description": "Find prior assistant answers matching the supplied question terms, for read-only agent memory recall.",
			"annotations": readOnlyAnnotations("Find similar question"),
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"query": strSchema("question or distinctive terms"), "limit": intSchema("maximum sessions, default 20"),
			}, "required": []string{"query"}},
		},
		{
			"name":        "get_session",
			"description": "Fetch one session's metadata and final result. pattern may be a full sessionId, a fragment of one, or a fragment of the file path.",
			"annotations": readOnlyAnnotations("Get session"),
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": strSchema("a sessionId, a fragment of one, or a file path fragment"),
					"source":  strSchema("restrict the match to one source (ids are not unique across sources): " + strings.Join(knownModes, " / ")),
				},
				"required": []string{"pattern"},
			},
		},
		{
			"name":        "get_messages",
			"description": "Fetch a session's messages. The interesting part of a long session is usually its end, so use order=desc for the latest N. role narrows to the human intent (user) or the answers (assistant).",
			"annotations": readOnlyAnnotations("Get messages"),
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": strSchema("a sessionId, a fragment of one, or a file path fragment"),
					"source":  strSchema("restrict the match to one source (ids are not unique across sources): " + strings.Join(knownModes, " / ")),
					"limit":   intSchema("how many to return at most, default 50"),
					"order":   strSchema("asc for the earliest N (default), desc for the latest N"),
					"role":    strSchema("keep only this role: user or assistant"),
					"at":      strSchema("anchor the window at this time instead of at an end — pass a hit's timestamp from search_sessions to land on it: asc starts there, desc ends there"),
					"cursor":  cursorSchema(),
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
