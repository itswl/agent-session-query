package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sort"
)

// link resolves what no single transcript knows: which transcript a subagent ran in and
// how deep it sits, how many ran beneath each session, and which transcripts are copies
// of another.
func (ix *Index) link(ctx context.Context) error {
	type node struct {
		sid                         int64
		source, id, key             string
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
SELECT sid, source, id, key, parent_session, parent_agent, first_id, last_id,
       parent_sid, root_sid, copy_of, depth, agents, items, updated_at
FROM sessions`)
	if err != nil {
		return err
	}
	var nodes []*node
	for rows.Next() {
		n := &node{}
		if err := rows.Scan(&n.sid, &n.source, &n.id, &n.key, &n.parentSession, &n.parentAgent, &n.firstID, &n.lastID,
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
	// A copy of a copy is a copy of the original
	resolve := func(n *node) *node {
		bySid := map[int64]*node{}
		for _, m := range nodes {
			bySid[m.sid] = m
		}
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
	bySid := map[int64]*node{}
	for _, n := range nodes {
		bySid[n.sid] = n
	}
	for _, n := range nodes {
		if n.parentSid == 0 {
			continue
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
