package index

import (
	"context"
	"encoding/hex"
	"time"
)

// TurnStamp names one turn and what it held when the index last read it. An exporter
// keeps the hash of what it sent: a turn whose hash has moved is a turn to send again.
type TurnStamp struct {
	SID       int64
	Index     int
	Hash      string
	StartedAt time.Time
	LastAt    time.Time
}

// TurnStamps lists the turns of the sessions a filter selects that were asked within w
// and whose last row was written before quiet, oldest first. A turn still being written
// is left for a later pass: sent now it would be sent again in a minute.
func (ix *Index) TurnStamps(ctx context.Context, f Filter, w Window, quiet time.Time) ([]TurnStamp, error) {
	where, args := ix.widen(ctx, reportFilter(f), !w.Since.IsZero()).where()
	when, whenArgs := w.where("t.started_at")
	args = append(args, whenArgs...)
	q := `SELECT t.sid, t.idx, t.hash, t.started_at, max(t.last_at, t.ended_at, t.started_at)
FROM turns t JOIN sessions s ON s.sid = t.sid
WHERE ` + where + ` AND t.idx > 0 AND ` + when
	if !quiet.IsZero() {
		q += ` AND max(t.last_at, t.ended_at, t.started_at) <= ?`
		args = append(args, ms(quiet))
	}
	rows, err := ix.ro.QueryContext(ctx, q+` ORDER BY t.started_at, t.sid, t.idx`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TurnStamp
	for rows.Next() {
		var t TurnStamp
		var hash []byte
		var started, last int64
		if err := rows.Scan(&t.SID, &t.Index, &hash, &started, &last); err != nil {
			return nil, err
		}
		t.Hash, t.StartedAt, t.LastAt = hex.EncodeToString(hash), fromMs(started), fromMs(last)
		out = append(out, t)
	}
	return out, rows.Err()
}

// BySID finds a session by its row in this index.
func (ix *Index) BySID(ctx context.Context, sid int64) (Session, error) { return ix.bySID(ctx, sid) }

// Parent finds the transcript that spawned a subagent's — the session it ran in, or
// another agent of it — and reports false for a session, or for a subagent whose parent
// is not in the index.
func (ix *Index) Parent(ctx context.Context, s *Session) (Session, bool, error) {
	var parent int64
	if err := ix.ro.QueryRowContext(ctx, `SELECT parent_sid FROM sessions WHERE sid = ?`, s.sid).Scan(&parent); err != nil || parent == 0 {
		return Session{}, false, nil
	}
	p, err := ix.bySID(ctx, parent)
	if err != nil {
		return Session{}, false, nil
	}
	return p, true, nil
}
