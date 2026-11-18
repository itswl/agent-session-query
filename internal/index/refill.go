package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/itswl/agent-session-query/internal/model"
)

// refill writes again, from what the index holds, the words of the turns words_stale
// names: every turn, once, after the words table was made anew (see newWords), and since
// then the turns another asq wrote (see wordsStale). No transcript is read for it: a turn's
// items are in the index, and what it keeps of them as a preview is kept whole beside it.
// The latest sessions first, whole, a few hundred turns to a transaction: a transaction
// each would cost more than the words, and one for all would hold every other writer
// back. done is called for each session written.
func (ix *Index) refill(ctx context.Context, done func()) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := ix.rw.QueryContext(ctx, `SELECT DISTINCT sid FROM words_stale ORDER BY sid DESC LIMIT ?`, refillSessions)
		if err != nil {
			return err
		}
		var sids []int64
		for rows.Next() {
			var sid int64
			if err := rows.Scan(&sid); err != nil {
				rows.Close()
				return err
			}
			sids = append(sids, sid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(sids) == 0 {
			return nil
		}
		written := 0
		err = ix.tx(ctx, func(tx *sql.Tx) error {
			written = 0
			for turns := 0; written < len(sids) && turns < refillTurns; written++ {
				n, err := ix.refillSession(ctx, tx, sids[written])
				if err != nil {
					return err
				}
				turns += n
			}
			return nil
		})
		if err != nil {
			return err
		}
		for range written {
			done()
		}
	}
}

// What one transaction of refill takes on: sessions whole, until they add up to this many
// turns. refillBatch is how many turns are read from the index at once.
const (
	refillSessions = 64
	refillTurns    = 500
	refillBatch    = 64
)

// refillSession writes the words of a session's stale turns, clears them from the table,
// and says how many it wrote.
func (ix *Index) refillSession(ctx context.Context, tx *sql.Tx, sid int64) (int, error) {
	var title string
	err := tx.QueryRowContext(ctx, `SELECT title FROM sessions WHERE sid = ?`, sid).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) { // the session is gone, and its turns with it
		_, err = tx.ExecContext(ctx, `DELETE FROM words_stale WHERE sid = ?`, sid)
		return 0, err
	}
	if err != nil {
		return 0, err
	}
	type stale struct {
		idx  int
		body []byte // nil: the turn is gone
	}
	written := 0
	for after := -1; ; {
		rows, err := tx.QueryContext(ctx, `
SELECT w.idx, t.body FROM words_stale w LEFT JOIN turns t ON t.sid = w.sid AND t.idx = w.idx
WHERE w.sid = ? AND w.idx > ? ORDER BY w.idx LIMIT ?`, sid, after, refillBatch)
		if err != nil {
			return 0, err
		}
		var batch []stale
		for rows.Next() {
			var s stale
			if err := rows.Scan(&s.idx, &s.body); err != nil {
				rows.Close()
				return 0, err
			}
			batch = append(batch, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		if len(batch) == 0 {
			break
		}
		for _, s := range batch {
			row := turnRow{idx: s.idx}
			if s.body != nil {
				// A turn that cannot be decoded has no words, rather than holding up every
				// other: it is written whole again when its transcript next changes
				if items, err := ix.wholeItems(ctx, tx, sid, s.idx, s.body); err == nil {
					row.words = turnWords(items)
				}
			}
			tw := ""
			if s.idx == titleTurn {
				tw = titleWords(title)
			}
			if err := putWords(ctx, tx, sid, &row, tw, true); err != nil {
				return 0, err
			}
			after = s.idx
			written++
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM words_stale WHERE sid = ?`, sid)
	return written, err
}

// wholeItems decodes a turn's items, with the text each keeps as a preview read whole.
func (ix *Index) wholeItems(ctx context.Context, tx *sql.Tx, sid int64, idx int, body []byte) ([]model.Item, error) {
	raw, err := ix.dec.DecodeAll(body, nil)
	if err != nil {
		return nil, err
	}
	var b turnBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, part, data FROM bodies WHERE sid = ? AND idx = ?`, sid, idx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	at := map[int]*model.Item{}
	for i := range b.Items {
		at[b.Items[i].Seq] = &b.Items[i]
	}
	for rows.Next() {
		var seq int
		var part string
		var data []byte
		if err := rows.Scan(&seq, &part, &data); err != nil {
			return nil, err
		}
		if it := at[seq]; it != nil {
			ix.restore(it, part, data)
		}
	}
	return b.Items, rows.Err()
}
