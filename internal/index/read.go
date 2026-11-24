package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/itswl/agent-session-query/internal/model"
)

// Outline is a session's turns without their items: what was asked and how each ended.
// Turn 0 — what came before the first ask — is listed only when there is one.
func (ix *Index) Outline(ctx context.Context, s *Session) ([]model.Turn, error) {
	passed, err := ix.passed(ctx, s.sid, 0, -1)
	if err != nil {
		return nil, err
	}
	rows, err := ix.ro.QueryContext(ctx, `SELECT head, items FROM turns WHERE sid = ? ORDER BY idx`, s.sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Turn{}
	for rows.Next() {
		var head string
		var steps int
		if err := rows.Scan(&head, &steps); err != nil {
			return nil, err
		}
		var t model.Turn
		if err := json.Unmarshal([]byte(head), &t); err != nil {
			return nil, err
		}
		t.Steps, t.PassedLater = steps, passed[t.Index]
		s.notEnded(&t)
		out = append(out, t)
	}
	return out, rows.Err()
}

// TurnRange selects turns by index, both ends included. Last below zero means "to the
// end"; Tail, when set, keeps only that many of the latest in the range.
type TurnRange struct {
	First int
	Last  int
	Tail  int
	// Full reads every item whole rather than as the preview the turn keeps.
	Full bool
}

// Turns reads a session's turns with their items. Long text comes as a preview unless
// the range asks for it whole: an item's Bytes (or its tool's OutBytes / InBytes) says
// how much there is, and Item reads one whole.
func (ix *Index) Turns(ctx context.Context, s *Session, r TurnRange) ([]model.Turn, error) {
	q, args := `SELECT idx, head, body FROM turns WHERE sid = ? AND idx >= ?`, []any{s.sid, r.First}
	if r.Last >= 0 {
		q += ` AND idx <= ?`
		args = append(args, r.Last)
	}
	if r.Tail > 0 {
		q = `SELECT idx, head, body FROM (` + q + ` ORDER BY idx DESC LIMIT ?) ORDER BY idx`
		args = append(args, r.Tail)
	} else {
		q += ` ORDER BY idx`
	}
	rows, err := ix.ro.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Turn{}
	for rows.Next() {
		var idx int
		var head string
		var blob []byte
		if err := rows.Scan(&idx, &head, &blob); err != nil {
			return nil, err
		}
		t, err := ix.decodeTurn(head, blob)
		if err != nil {
			return nil, err
		}
		s.notEnded(&t)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		passed, err := ix.passed(ctx, s.sid, out[0].Index, out[len(out)-1].Index)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].PassedLater = passed[out[i].Index]
		}
	}
	if r.Full {
		for i := range out {
			if err := ix.fill(ctx, s.sid, &out[i]); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (ix *Index) decodeTurn(head string, blob []byte) (model.Turn, error) {
	var t model.Turn
	if err := json.Unmarshal([]byte(head), &t); err != nil {
		return t, err
	}
	raw, err := ix.dec.DecodeAll(blob, nil)
	if err != nil {
		return t, err
	}
	var body turnBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return t, err
	}
	t.Items, t.Calls, t.Steps = body.Items, body.Calls, len(body.Items)
	return t, nil
}

// fill replaces every preview in a turn with the whole text.
func (ix *Index) fill(ctx context.Context, sid int64, t *model.Turn) error {
	rows, err := ix.ro.QueryContext(ctx, `SELECT seq, part, data FROM bodies WHERE sid = ? AND idx = ?`, sid, t.Index)
	if err != nil {
		return err
	}
	defer rows.Close()
	at := map[int]*model.Item{}
	for i := range t.Items {
		at[t.Items[i].Seq] = &t.Items[i]
	}
	for rows.Next() {
		var seq int
		var part string
		var data []byte
		if err := rows.Scan(&seq, &part, &data); err != nil {
			return err
		}
		if it := at[seq]; it != nil {
			ix.restore(it, part, data)
		}
	}
	return rows.Err()
}

func (ix *Index) restore(it *model.Item, part string, data []byte) {
	raw, err := ix.dec.DecodeAll(data, nil)
	if err != nil {
		return
	}
	switch part {
	case "text":
		it.Text, it.Bytes = string(raw), 0
	case "output":
		if it.Tool != nil {
			it.Tool.Output, it.Tool.OutBytes = string(raw), 0
		}
	case "input":
		if it.Tool != nil {
			it.Tool.Input, it.Tool.InBytes = raw, 0
		}
	}
}

// Input reads a tool call's input whole, where the turn keeps a preview of it (its
// InBytes is set): only that, not the turn it is in. ok is false when the index holds no
// more of it than the preview.
func (ix *Index) Input(ctx context.Context, s *Session, seq int) (input json.RawMessage, ok bool, err error) {
	var data []byte
	err = ix.ro.QueryRowContext(ctx, `SELECT data FROM bodies WHERE sid = ? AND seq = ? AND part = 'input'`, s.sid, seq).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	raw, err := ix.dec.DecodeAll(data, nil)
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// ErrNoItem says a session holds no item at that position.
var ErrNoItem = errors.New("no such item")

// Item reads one item whole, and says which turn it is in.
func (ix *Index) Item(ctx context.Context, s *Session, seq int) (model.Item, int, error) {
	var idx int
	var head string
	var blob []byte
	err := ix.ro.QueryRowContext(ctx, `
SELECT idx, head, body FROM turns WHERE sid = ? AND first_seq <= ? AND items > 0 ORDER BY first_seq DESC LIMIT 1`, s.sid, seq).Scan(&idx, &head, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Item{}, 0, ErrNoItem
	}
	if err != nil {
		return model.Item{}, 0, err
	}
	t, err := ix.decodeTurn(head, blob)
	if err != nil {
		return model.Item{}, 0, err
	}
	for i := range t.Items {
		if t.Items[i].Seq != seq {
			continue
		}
		it := t.Items[i]
		rows, err := ix.ro.QueryContext(ctx, `SELECT part, data FROM bodies WHERE sid = ? AND seq = ?`, s.sid, seq)
		if err != nil {
			return it, idx, err
		}
		defer rows.Close()
		for rows.Next() {
			var part string
			var data []byte
			if err := rows.Scan(&part, &data); err != nil {
				return it, idx, err
			}
			ix.restore(&it, part, data)
		}
		return it, idx, rows.Err()
	}
	return model.Item{}, 0, ErrNoItem
}

// TurnAt is the index of the turn a moment falls in: the last turn that had started by
// then. Zero when the moment precedes the first ask.
func (ix *Index) TurnAt(ctx context.Context, s *Session, atMs int64) (int, error) {
	var idx int
	err := ix.ro.QueryRowContext(ctx, `
SELECT idx FROM turns WHERE sid = ? AND idx > 0 AND started_at <= ? ORDER BY idx DESC LIMIT 1`, s.sid, atMs).Scan(&idx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return idx, err
}
