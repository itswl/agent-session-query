package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/itswl/agent-session-query/internal/command"
	"github.com/itswl/agent-session-query/internal/model"
	"github.com/itswl/agent-session-query/internal/txt"
)

// Whether a failed command passed later is worked out once, when its session is read, and
// kept beside the failure: the report of failures, a session's turns, the list of sessions
// and the digest each say it, and each would otherwise read the turns after every failure
// it shows to learn it.
//
// "The command" is the program and what it was asked to do — go test, pnpm build, git
// push — read past what prepares for it (see command.Key). An agent rarely runs a failed
// command again word for word: it narrows the test, adds a flag. Measured on a real
// history, the same command word for word came back in none of 37 failures and the same
// program and subcommand in 28 of 40, all within three turns. The match is loose, so what
// passed is kept whole beside it: the reader judges whether "go test ./internal/x"
// passing says anything about "go test ./..." failing.
//
// Succeeded is taken strictly (see command.Passed). A run begun before the failure was
// known — beside it, in the same response — is not later than it. Another session is not
// looked in.

// laterTurns is how many turns after a failure are looked through for the same command
// succeeding. The triggers of failuresJudged name it too.
const laterTurns = 3

// run is a tool call that ran a command whose program is known: what a failure's later
// success is looked for among.
type run struct {
	turn, seq int
	call      int       // the response it was asked for in (model.Item.Call)
	at        time.Time // when it was called
	end       time.Time // when its answer came, or failing that, at: a failure's moment
	tool, key string
	command   string
	done      bool // answered
	passed    bool // and said its command succeeded
}

// runsOf are a turn's calls that ran a command, in order.
func runsOf(turn int, items []model.Item) []run {
	var out []run
	for i := range items {
		it := &items[i]
		t := it.Tool
		if t == nil || t.Command == "" {
			continue
		}
		key := command.Key(t.Command)
		if key == "" {
			continue // no command that comes back: code given inline, a line of shell syntax
		}
		out = append(out, run{turn: turn, seq: it.Seq, call: it.Call, at: it.At, end: firstTime(t.EndAt, it.At),
			tool: t.Name, key: key, command: t.Command, done: t.Done, passed: t.Done && command.Passed(t)})
	}
	return out
}

// laterOf finds where the command the failure f ran next succeeded, among runs: a
// session's, in order.
func laterOf(f *run, runs []run) *Later {
	i := sort.Search(len(runs), func(i int) bool { return runs[i].seq > f.seq })
	for ; i < len(runs) && runs[i].turn <= f.turn+laterTurns; i++ {
		r := &runs[i]
		if r.turn < f.turn || r.tool != f.tool || r.key != f.key {
			continue
		}
		if r.turn == f.turn && f.call != 0 && r.call == f.call {
			continue // asked for in the same response: run beside the failure, not after it
		}
		if !f.end.IsZero() && !r.at.IsZero() && r.at.Before(f.end) {
			continue // begun before the failure was known
		}
		if !r.done {
			return nil // run again, and never answered: nothing is known of it
		}
		if !r.passed {
			continue // failed again, or ended in a way that says nothing: the next run may be the one
		}
		return &Later{Turn: r.turn, Seq: r.seq, At: r.at, Command: txt.Truncate(strings.Join(strings.Fields(r.command), " "), 2000, "…")}
	}
	return nil
}

// runAt is the run at seq, if there is one.
func runAt(runs []run, seq int) *run {
	i := sort.Search(len(runs), func(i int) bool { return runs[i].seq >= seq })
	if i < len(runs) && runs[i].seq == seq {
		return &runs[i]
	}
	return nil
}

// judge works out, for every failure of a reading, the command it ran and where that next
// succeeded. The reading holds the whole transcript, so the turns after a failure are at
// hand, whichever of them changed.
func (rd *reading) judge() {
	var runs []run
	for i := range rd.turns {
		runs = append(runs, rd.turns[i].runs...)
		rd.turns[i].runs = nil
	}
	for i := range rd.turns {
		for j := range rd.turns[i].failed {
			f := &rd.turns[i].failed[j]
			if r := runAt(runs, f.seq); r != nil {
				f.key, f.later = r.key, laterOf(r, runs)
			}
		}
	}
}

// judged writes again what the reading says of the session's failures that are no longer
// judged — those of the turns before one written now (see failuresJudged) — or that no
// asq judged.
func judged(ctx context.Context, tx *sql.Tx, sid int64, rd *reading) error {
	rows, err := tx.QueryContext(ctx, `SELECT seq FROM failures WHERE sid = ? AND judged < ?`, sid, laterVersion)
	if err != nil {
		return err
	}
	var seqs []int
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return err
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(seqs) == 0 {
		return err
	}
	bySeq := map[int]*failureRow{}
	for i := range rd.turns {
		for j := range rd.turns[i].failed {
			bySeq[rd.turns[i].failed[j].seq] = &rd.turns[i].failed[j]
		}
	}
	for _, seq := range seqs {
		if f := bySeq[seq]; f != nil {
			if err := putJudgement(ctx, tx, sid, seq, f.key, f.later); err != nil {
				return err
			}
		}
	}
	return nil
}

// putJudgement writes what is known of one failure: the command it ran, and where that
// next succeeded, if it did.
func putJudgement(ctx context.Context, tx *sql.Tx, sid int64, seq int, key string, l *Later) error {
	var later Later
	if l != nil {
		later = *l
	}
	_, err := tx.ExecContext(ctx, `
UPDATE failures SET command_key = ?, later_idx = ?, later_seq = ?, later_at = ?, later_command = ?, judged = ? WHERE sid = ? AND seq = ?`,
		key, later.Turn, later.Seq, ms(later.At), later.Command, laterVersion, sid, seq)
	return err
}

// judge works out again, from what the index holds, whether the failures no asq of this
// laterVersion has judged passed later: every one, once, after an asq that did not judge
// them, or judged them otherwise, wrote them; and since then the failures of the turns
// before one another asq wrote (see failuresJudged). No transcript is read for it: the
// turns are in the index, and what a run is judged by — its command, how it ended — is
// kept whole in them. The latest sessions first, a few hundred failures to a transaction.
// done is called for each session judged.
func (ix *Index) judge(ctx context.Context, done func()) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := ix.rw.QueryContext(ctx, `SELECT DISTINCT sid FROM failures WHERE judged < ? ORDER BY sid DESC LIMIT ?`, laterVersion, refillSessions)
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
			for n := 0; written < len(sids) && n < refillTurns; written++ {
				m, err := ix.judgeSession(ctx, tx, sids[written])
				if err != nil {
					return err
				}
				n += m
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

// judgeSession judges a session's failures that are not, from its turns as the index holds
// them, and says how many it judged.
func (ix *Index) judgeSession(ctx context.Context, tx *sql.Tx, sid int64) (int, error) {
	type failure struct {
		seq, idx int
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, idx FROM failures WHERE sid = ? AND judged < ? ORDER BY seq`, sid, laterVersion)
	if err != nil {
		return 0, err
	}
	var fails []failure
	need := map[int]bool{}
	for rows.Next() {
		var f failure
		if err := rows.Scan(&f.seq, &f.idx); err != nil {
			rows.Close()
			return 0, err
		}
		fails = append(fails, f)
		for i := f.idx; i <= f.idx+laterTurns; i++ {
			need[i] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	turns := make([]int, 0, len(need))
	for i := range need {
		turns = append(turns, i)
	}
	sort.Ints(turns)
	// The turns needed, read in runs of consecutive ones. A turn that cannot be decoded
	// has no runs, rather than holding up every other: it is judged again when its
	// transcript next changes
	var runs []run
	for i := 0; i < len(turns); {
		j := i
		for j+1 < len(turns) && turns[j+1] == turns[j]+1 {
			j++
		}
		rows, err := tx.QueryContext(ctx, `SELECT idx, body FROM turns WHERE sid = ? AND idx >= ? AND idx <= ? ORDER BY idx`, sid, turns[i], turns[j])
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var idx int
			var body []byte
			if err := rows.Scan(&idx, &body); err != nil {
				rows.Close()
				return 0, err
			}
			if items, err := ix.items(body); err == nil {
				runs = append(runs, runsOf(idx, items)...)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		i = j + 1
	}
	for _, f := range fails {
		key, later := "", (*Later)(nil)
		if r := runAt(runs, f.seq); r != nil {
			key, later = r.key, laterOf(r, runs)
		}
		if err := putJudgement(ctx, tx, sid, f.seq, key, later); err != nil {
			return 0, err
		}
	}
	return len(fails), nil
}

// items decodes the items of a turn's body, as the index keeps them.
func (ix *Index) items(body []byte) ([]model.Item, error) {
	raw, err := ix.dec.DecodeAll(body, nil)
	if err != nil {
		return nil, err
	}
	var b turnBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return b.Items, nil
}

// passed counts, by turn, the failures of a session's turns first..last (last below zero:
// to the end) that the same command passed later.
func (ix *Index) passed(ctx context.Context, sid int64, first, last int) (map[int]int, error) {
	q, args := `SELECT idx, count(*) FROM failures WHERE sid = ? AND later_seq > 0 AND idx >= ?`, []any{sid, first}
	if last >= 0 {
		q += ` AND idx <= ?`
		args = append(args, last)
	}
	rows, err := ix.ro.QueryContext(ctx, q+` GROUP BY idx`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var idx, n int
		if err := rows.Scan(&idx, &n); err != nil {
			return nil, err
		}
		out[idx] = n
	}
	return out, rows.Err()
}

// PassedLater is how many of a turn's failed calls the same command passed later, in that
// turn or the three after it (see Failure.Later).
func (ix *Index) PassedLater(ctx context.Context, s *Session, turn int) (int, error) {
	by, err := ix.passed(ctx, s.sid, turn, turn)
	return by[turn], err
}
