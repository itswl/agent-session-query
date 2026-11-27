package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/gzip"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/index"
	"github.com/itswl/agent-session-query/internal/render"
)

// The most one page of a list holds. A caller that wants more asks for the next page.
const maxLimit = 500

var gzips = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed); return w }}

// writeBody sends a reply, compressed when the caller takes that and it is worth it: a
// long turn is megabytes of text that shrinks to a tenth.
func writeBody(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	h := w.Header()
	h.Add("Vary", "Accept-Encoding")
	if len(body) >= 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && !strings.HasPrefix(h.Get("Content-Type"), "font/") {
		zw := gzips.Get().(*gzip.Writer)
		defer gzips.Put(zw)
		var buf bytes.Buffer
		buf.Grow(len(body) / 4)
		zw.Reset(&buf)
		_, _ = zw.Write(body)
		if zw.Close() == nil {
			h.Set("Content-Encoding", "gzip")
			body = buf.Bytes()
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// writeJSON sends a reply as JSON that nothing can take for markup. A reply carries
// transcripts, which are other people's text, and quotes what the caller sent — a path
// that matched nothing, an Origin that is not accepted. json.Marshal writes <, > and &
// as \u003c, \u003e and \u0026, so there is no tag in the body whatever reads it: the
// content type and nosniff say it is not a page, and this makes it not one.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"the reply could not be encoded"}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store") // the record is private and changes under the reader
	writeBody(w, r, status, append(body, '\n'))
}

func writeError(w http.ResponseWriter, r *http.Request, status int, message string) {
	writeJSON(w, r, status, map[string]any{"error": message})
}

// fail answers a request that went wrong, with the status the reason deserves.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var amb *index.AmbiguousError
	var bad badRequest
	var refused app.Refused
	switch {
	case errors.Is(err, index.ErrNotFound):
		writeError(w, r, http.StatusNotFound, err.Error())
	case errors.As(err, &amb), errors.As(err, &bad), errors.As(err, &refused):
		writeError(w, r, http.StatusBadRequest, err.Error())
	case r.Context().Err() != nil:
		// the caller left; nobody reads this
		w.WriteHeader(499)
	default:
		s.logf("%s %s: %v", r.Method, r.URL.Path, err)
		// Not the index's alone to say: since why, a 500 may be git's, which the index never heard of
		msg := "asq could not answer: " + err.Error()
		if s.app.Index.Damage(err) {
			msg += " — the index is damaged, and is set aside and made anew from the stores within seconds"
		}
		writeError(w, r, http.StatusInternalServerError, msg)
	}
}

// badRequest marks an error as the caller's: a filter that does not parse. app.Refused is
// the caller's as well, and needs no marking: a line why was asked for that is not there.
type badRequest struct{ error }

func flagOf(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func intOf(r *http.Request, name string, def int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def, badRequest{fmt.Errorf("%s: want a whole number, got %q", name, raw)}
	}
	return n, nil
}

func pageOf(r *http.Request, def int) (index.Page, error) {
	limit, err := intOf(r, "limit", def)
	if err != nil {
		return index.Page{}, err
	}
	offset, err := intOf(r, "offset", 0)
	if err != nil {
		return index.Page{}, err
	}
	if limit == 0 {
		limit = def
	}
	return index.Page{Limit: min(limit, maxLimit), Offset: offset}, nil
}

func queryOf(r *http.Request) app.Query {
	v := r.URL.Query()
	return app.Query{
		Source: v.Get("source"), Project: v.Get("project"), Branch: v.Get("branch"), Text: v.Get("text"),
		Since: v.Get("since"), Until: v.Get("until"), Archived: flagOf(r, "archived"), Agents: flagOf(r, "agents"),
	}
}

// selection reads a request's filter and the time window its reports cover.
func (s *Server) selection(r *http.Request, defaultSince string) (index.Filter, index.Window, error) {
	q := queryOf(r)
	f, err := s.app.Filter(q)
	if err != nil {
		return f, index.Window{}, badRequest{err}
	}
	w, err := s.app.Window(q, defaultSince)
	if err != nil {
		return f, w, badRequest{err}
	}
	return f, w, nil
}

// session finds the session a route names.
func (s *Server) session(r *http.Request) (index.Session, error) {
	sess, _, err := s.app.Resolve(r.Context(), r.PathValue("source")+"/"+r.PathValue("id"))
	return sess, err
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h, err := s.app.Health(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// What the guards did is for those they let in, as the rest of the report is
	writeJSON(w, r, http.StatusOK, struct {
		app.Health
		Guard guardCounts `json:"guard"`
	}{h, s.door.counts(time.Now())})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	q := queryOf(r)
	q.State = r.URL.Query().Get("state") // the list's own: where the latest turn stands
	f, err := s.app.Filter(q)
	if err != nil {
		s.fail(w, r, badRequest{err})
		return
	}
	page, err := pageOf(r, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list, total, err := s.app.Index.Sessions(r.Context(), f, page)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"sessions": app.Listed(list), "total": total, "limit": page.Limit, "offset": page.Offset})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	sess, err := s.session(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	turns, err := s.app.Index.Outline(r.Context(), &sess)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	agents, err := s.app.Index.Children(r.Context(), &sess)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	above, err := s.app.Above(r.Context(), &sess)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"session": sess, "turns": turns, "agents": app.Listed(agents), "above": above})
}

// handleKeep keeps a session one by one, or stops (see internal/keep): the one thing the
// API changes, and only in asq's own state, never in a CLI's store.
func (s *Server) handleKeep(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keep *bool `json:"keep"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Keep == nil {
		writeError(w, r, http.StatusBadRequest, `say whether to keep it: {"keep": true} or {"keep": false}`)
		return
	}
	sess, err := s.session(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if why := app.Unkeepable(&sess); why != "" && (*body.Keep || sess.Kept != "copy") {
		writeError(w, r, http.StatusConflict, sess.Ref+" is not kept one by one: "+why)
		return
	}
	k, err := s.app.KeepSession(r.Context(), &sess, *body.Keep)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if sess, err = s.session(r); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"session": sess, "keeping": k})
}

func (s *Server) handleTurns(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	spec := r.URL.Query().Get("turns")
	if spec == "" {
		spec = "last"
	}
	rng, _, err := app.TurnRange(spec, -1)
	if err != nil {
		s.fail(w, r, badRequest{err})
		return
	}
	rng.Full = flagOf(r, "full")
	turns, err := s.app.Index.Turns(r.Context(), &sess, rng)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"session": sess.Ref, "turns": turns})
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	seq, err := strconv.Atoi(r.PathValue("seq"))
	if err != nil || seq < 0 {
		s.fail(w, r, badRequest{fmt.Errorf("a step is named by its number, not %q", r.PathValue("seq"))})
		return
	}
	item, turn, err := s.app.Index.Item(r.Context(), &sess, seq)
	if err != nil {
		if errors.Is(err, index.ErrNoItem) {
			err = fmt.Errorf("%w: %s has no step %d", index.ErrNotFound, sess.Ref, seq)
		}
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"session": sess.Ref, "turn": turn, "item": item})
}

// handlePicture sends a picture a step carried: PNG, JPEG, GIF or WebP, by its bytes, read
// from the transcript. It is sent as what it is and nothing else can be made of it: its
// type is the one its bytes have, and opened on its own it is a sandboxed page that loads
// nothing. Like every reply it is not kept: the page keeps it for as long as it is open.
func (s *Server) handlePicture(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	seq, err1 := strconv.Atoi(r.PathValue("seq"))
	n, err2 := strconv.Atoi(r.PathValue("n"))
	if err1 != nil || err2 != nil || seq < 0 || n < 0 {
		s.fail(w, r, badRequest{fmt.Errorf("a step's file is named by two numbers, not %q and %q", r.PathValue("seq"), r.PathValue("n"))})
		return
	}
	pic, err := s.app.Picture(r.Context(), &sess, seq, n)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", pic.Type)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Content-Length", strconv.Itoa(len(pic.Data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(pic.Data)
	}
}

func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	sess, err := s.session(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	turn, err := intOf(r, "turn", 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	from, err := intOf(r, "from", 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	opts, err := app.BriefOf(turn, from, r.URL.Query().Get("since"))
	if err != nil {
		s.fail(w, r, badRequest{err})
		return
	}
	text, err := s.app.Brief(r.Context(), &sess, opts)
	if err != nil {
		s.fail(w, r, badRequest{err})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"session": sess.Ref, "brief": text})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	sess, err := s.session(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = render.FormatMarkdown
	}
	turns, err := s.app.Index.Turns(r.Context(), &sess, index.TurnRange{First: 0, Last: -1, Full: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	text, err := render.Export(&sess, turns, format)
	if err != nil {
		s.fail(w, r, badRequest{err})
		return
	}
	h := w.Header()
	h.Set("Content-Type", render.ContentType(format))
	h.Set("Cache-Control", "no-store")
	// Always a download. An exported page is a transcript's text laid out as HTML, and
	// opened on this origin it would be this origin's page; saved, it is a file.
	h.Set("Content-Disposition", `attachment; filename="`+fileName(&sess, format)+`"`)
	h.Set("Content-Security-Policy", "sandbox")
	writeBody(w, r, http.StatusOK, []byte(text))
}

// fileName names an export after its session, in characters every file system takes.
func fileName(sess *index.Session, format string) string {
	id := sess.ID
	if len(id) > 12 {
		id = id[:12]
	}
	var b strings.Builder
	for _, c := range sess.Source + "-" + id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	ext := map[string]string{render.FormatMarkdown: "md", render.FormatJSON: "json", render.FormatJSONL: "jsonl", render.FormatHTML: "html"}[format]
	return b.String() + "." + ext
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	q := queryOf(r)
	q.Agents = true // a subagent's turns are found with the rest
	// session confines the search to one session's own turns, as the page's find in a
	// session asks. Named, it is searched whether or not it is archived; its subagents'
	// transcripts are sessions of their own, and are not in it.
	var one index.Session
	ref := r.URL.Query().Get("session")
	if ref != "" {
		var err error
		if one, _, err = s.app.Resolve(r.Context(), ref); err != nil {
			s.fail(w, r, err)
			return
		}
		q.Archived = true
	}
	f, err := s.app.Filter(q)
	if err != nil {
		s.fail(w, r, badRequest{err})
		return
	}
	f.SID = one.SID()
	page, err := pageOf(r, 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Find in a session wants all of it: the bound on turns of one session, which keeps one
	// session from filling a page, is not put on a search of one session unless it is asked —
	// as asq search --session and the MCP server's search take it
	bound := 3
	if ref != "" {
		bound = -1
	}
	per, err := intOf(r, "per_session", bound)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if per == 0 {
		per = -1
	}
	res, err := s.app.Search(r.Context(), index.ParseQuery(r.URL.Query().Get("q")), f,
		index.SearchOptions{Limit: min(page.Limit, 100), Offset: page.Offset, PerSession: per, Recent: flagOf(r, "recent")})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, app.ListedHits(res))
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	f, _, err := s.selection(r, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list, err := s.app.Index.Projects(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"projects": list})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	f, win, err := s.selection(r, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if by := r.URL.Query().Get("by"); by != "" {
		limit, err := intOf(r, "limit", 50)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		rows, err := s.app.Index.Usage(r.Context(), f, win, by, min(limit, maxLimit))
		if err != nil {
			s.fail(w, r, badRequest{err})
			return
		}
		total, err := s.app.Index.UsageTotal(r.Context(), f, win)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, r, http.StatusOK, map[string]any{"by": by, "rows": rows, "total": total})
		return
	}
	rep, err := s.app.Index.UsageSummary(r.Context(), f, win)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, rep)
}

func (s *Server) handleFailures(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	f, win, err := s.selection(r, "7d")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := pageOf(r, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := index.FailureQuery{Tool: r.URL.Query().Get("tool"), Text: r.URL.Query().Get("q"), Open: flagOf(r, "open")}
	var rep any
	switch by := r.URL.Query().Get("by"); by {
	case "":
		rep, err = s.app.Index.Failures(r.Context(), f, win, q, page)
	case "command":
		rep, err = s.app.Index.FailuresByCommand(r.Context(), f, win, q, page)
	default:
		err = badRequest{fmt.Errorf("failures are grouped by command, not by %q", by)}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, rep)
}

func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	f, win, err := s.selection(r, "7d")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.app.Index.Digest(r.Context(), f, win)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, rep)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	f, win, err := s.selection(r, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := pageOf(r, 50)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list, total, err := s.app.Files(r.Context(), f, win, r.URL.Query().Get("path"), flagOf(r, "changed"), page)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"files": list, "total": total})
}

// handleWhy answers which session wrote one line of a file. Over HTTP the path is held to
// app.WhyOverHTTP's rule, as the MCP endpoint holds it. What is wrong with the question is
// the caller's, 400; git missing from this machine is the server's own, 500.
func (s *Server) handleWhy(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	q := r.URL.Query()
	file, said := strings.TrimSpace(q.Get("path")), strings.TrimSpace(q.Get("line"))
	if file == "" || said == "" {
		s.fail(w, r, badRequest{fmt.Errorf("want path=<file> and line=<n>, counted from 1: got path=%q line=%q", file, said)})
		return
	}
	line, err := strconv.Atoi(said) // not intOf: a line is counted from 1, and 0 is not one
	if err != nil || line < 1 {
		s.fail(w, r, badRequest{fmt.Errorf("line: want a whole number, counted from 1, got %q", said)})
		return
	}
	if why, err := s.app.WhyOverHTTP(r.Context(), file, line); err != nil {
		s.fail(w, r, err)
	} else {
		writeJSON(w, r, http.StatusOK, why)
	}
}

func (s *Server) handlePack(w http.ResponseWriter, r *http.Request) {
	s.fresh(r)
	limit, err := intOf(r, "limit", 20)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	text, n, err := s.app.Pack(r.Context(), queryOf(r), min(limit, 100), flagOf(r, "turns"))
	if err != nil {
		s.fail(w, r, badRequest{err})
		return
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"pack": text, "sessions": n})
}
