package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
)

// linkVersion numbers the rules link follows, in the meta table under "link". An index
// whose sessions an older asq linked — a lower number, or none — is linked again at the
// next sync, whether or not that reads a transcript: what the rules decide was decided
// the older way, and nothing else would have it decided again until a session changed.
//
// 1: a session two stores of one CLI hold is counted once.
const linkVersion = 1

// linkedBy is the linkVersion of the asq that last linked the index's sessions: 0 when the
// index does not say.
func linkedBy(ctx context.Context, q querier) (int, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'link'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	n, _ := strconv.Atoi(v)
	return n, err
}

// linked notes that the index's sessions are linked by this asq's rules.
func (ix *Index) linked(ctx context.Context) error {
	_, err := ix.rw.ExecContext(ctx, `INSERT OR REPLACE INTO meta (key, value) VALUES ('link', ?)`, strconv.Itoa(linkVersion))
	return err
}

// link resolves what no single transcript knows: which transcript a subagent ran in and
// how deep it sits, how many ran beneath each session, and which transcripts are copies
// of another.
func (ix *Index) link(ctx context.Context) error {
	type node struct {
		sid                         int64
		source, agent, id, key      string
		parentSession, parentAgent  string
		firstID, lastID             string
		parentSid, rootSid, copyOf  int64
		depth, agents               int
		wasParent, wasRoot, wasCopy int64
		wasDepth, wasAgents         int
		items                       int
		updatedAt                   int64
	}
	rows, err := ix.rw.QueryContext(ctx, `
SELECT sid, source, agent, id, key, parent_session, parent_agent, first_id, last_id,
       parent_sid, root_sid, copy_of, depth, agents, items, updated_at
FROM sessions`)
	if err != nil {
		return err
	}
	var nodes []*node
	for rows.Next() {
		n := &node{}
		if err := rows.Scan(&n.sid, &n.source, &n.agent, &n.id, &n.key, &n.parentSession, &n.parentAgent, &n.firstID, &n.lastID,
			&n.wasParent, &n.wasRoot, &n.wasCopy, &n.wasDepth, &n.wasAgents, &n.items, &n.updatedAt); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Sessions by id, and the agents of each session by their own id
	sessions := map[string]*node{} // source \x00 id → a session a person started
	agents := map[string]*node{}   // source \x00 parent session \x00 agent id
	for _, n := range nodes {
		if n.parentSession == "" {
			sessions[n.source+"\x00"+n.id] = n
		} else {
			agents[n.source+"\x00"+n.parentSession+"\x00"+n.id] = n
		}
	}

	// A copy: a transcript that begins where another does and ends on a message the
	// other also holds. Claude Code writes one when a session is forked or resumed into
	// a new file: every message keeps its id, the file gets a new session id. The one
	// that goes on past that message — or, when they end alike, the larger and older —
	// is the session; the copy is folded into it.
	groups := map[string][]*node{}
	for _, n := range nodes {
		if n.parentSession == "" && n.firstID != "" && n.lastID != "" {
			k := n.source + "\x00" + filepath.Dir(n.key) + "\x00" + n.firstID
			groups[k] = append(groups[k], n)
		}
	}
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		ids := map[int64]map[string]bool{}
		for _, n := range group {
			set, err := ix.itemIDs(ctx, n.sid)
			if err != nil {
				return err
			}
			ids[n.sid] = set
		}
		sort.Slice(group, func(i, j int) bool { return group[i].sid < group[j].sid })
		for _, n := range group {
			var best *node
			for _, other := range group {
				if other == n || !ids[other.sid][n.lastID] {
					continue
				}
				// Two that end on the same message are copies of each other: only the
				// lesser of them yields
				if other.lastID == n.lastID && !(other.items > n.items || (other.items == n.items && other.sid < n.sid)) {
					continue
				}
				if best == nil || other.items > best.items || (other.items == best.items && other.sid < best.sid) {
					best = other
				}
			}
			if best != nil {
				n.copyOf = best.sid
			}
		}
	}
	// A session two stores of one CLI both hold — the CLI's own and a backup of it beside
	// it, two copies of another machine's, the CLI's own and what asq keeps of it (see
	// internal/keep) — is one session, counted once: as the CLI's own store holds it while
	// it does, and once the CLI deleted it, as the store that holds the most of it. The
	// others are copies of that one. A subagent's transcript is the same one when it ran
	// in the same session and has the same id.
	//
	// counts says whether a is the one of two that counts: the one in the CLI's own store,
	// then the one that holds more of the session, then the one written later, then the
	// one read first.
	counts := func(a, b *node) bool {
		switch aOwn, bOwn := a.source == a.agent, b.source == b.agent; {
		case aOwn != bOwn:
			return aOwn
		case a.items != b.items:
			return a.items > b.items
		case a.updatedAt != b.updatedAt:
			return a.updatedAt > b.updatedAt
		}
		return a.sid < b.sid
	}
	held := map[string]*node{} // agent \x00 id → the session a person started, as the store that counts it holds it
	twins := map[string][]*node{}
	for _, n := range nodes {
		if n.id != "" {
			k := n.agent + "\x00" + n.parentSession + "\x00" + n.id
			twins[k] = append(twins[k], n)
		}
	}
	for _, group := range twins {
		best := group[0]
		for _, n := range group[1:] {
			if counts(n, best) {
				best = n
			}
		}
		for _, n := range group {
			if n.source != best.source {
				n.copyOf = best.sid
			}
		}
		if best.parentSession == "" {
			held[best.agent+"\x00"+best.id] = best
		}
	}

	// A copy of a copy is a copy of the original
	bySid := map[int64]*node{}
	for _, n := range nodes {
		bySid[n.sid] = n
	}
	resolve := func(n *node) *node {
		for hops := 0; n.copyOf != 0 && hops < len(nodes); hops++ {
			next, ok := bySid[n.copyOf]
			if !ok {
				break
			}
			n = next
		}
		return n
	}
	for _, n := range nodes {
		if n.copyOf != 0 {
			n.copyOf = resolve(n).sid
		}
	}

	// A subagent hangs from the agent that spawned it, or from the session itself; its
	// depth follows those links rather than what its meta file says, which starts from 0
	// for a team's members and from 1 for an agent a call spawned.
	for _, n := range nodes {
		if n.parentSession == "" {
			continue
		}
		root := sessions[n.source+"\x00"+n.parentSession]
		if root == nil {
			// The session it ran in, in another store of the CLI: what is kept of a session
			// whose CLI deleted its transcript and left its subagents'
			root = held[n.agent+"\x00"+n.parentSession]
		}
		if root != nil && root.copyOf != 0 {
			root = resolve(root) // the agents of a copy stay with the session that survives
		}
		if root == nil {
			continue
		}
		n.rootSid = root.sid
		n.parentSid = root.sid
		n.depth = 1
		at := n
		for hops := 0; at.parentAgent != "" && hops < 64; hops++ {
			up := agents[n.source+"\x00"+n.parentSession+"\x00"+at.parentAgent]
			if up == nil || up == at {
				break
			}
			if at == n {
				n.parentSid = up.sid
			}
			n.depth++
			at = up
		}
	}
	for _, n := range nodes {
		if n.parentSid == 0 || n.copyOf != 0 {
			continue // a copy of an agent's transcript is counted as the transcript it is a copy of
		}
		if p := bySid[n.parentSid]; p != nil {
			p.agents++
		}
		if n.rootSid != n.parentSid {
			if r := bySid[n.rootSid]; r != nil {
				r.agents++
			}
		}
	}

	return ix.tx(ctx, func(tx *sql.Tx) error {
		for _, n := range nodes {
			if n.parentSid == n.wasParent && n.rootSid == n.wasRoot && n.copyOf == n.wasCopy && n.depth == n.wasDepth && n.agents == n.wasAgents {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET parent_sid=?, root_sid=?, copy_of=?, depth=?, agents=? WHERE sid=?`,
				n.parentSid, n.rootSid, n.copyOf, n.depth, n.agents, n.sid); err != nil {
				return err
			}
		}
		return nil
	})
}

// itemIDs is every message id a transcript holds.
func (ix *Index) itemIDs(ctx context.Context, sid int64) (map[string]bool, error) {
	rows, err := ix.rw.QueryContext(ctx, `SELECT body FROM turns WHERE sid = ?`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		raw, err := ix.dec.DecodeAll(blob, nil)
		if err != nil {
			continue
		}
		var body struct {
			Items []struct {
				ID        string `json:"id"`
				Inherited bool   `json:"inherited"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &body) != nil {
			continue
		}
		for _, it := range body.Items {
			if it.ID != "" && !it.Inherited {
				out[it.ID] = true
			}
		}
	}
	return out, rows.Err()
}
