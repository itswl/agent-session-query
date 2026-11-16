package opencode

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/itswl/agent-session-query/internal/sources/kit"
	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// FuzzRead reads a database built from its input, a row to a line: a type, a tab, and the
// JSON OpenCode keeps in a row's data column. The lines are the messages of a 2.x session,
// and again of a fork that copied them, the older half as copies; and in the same database
// the messages ("message" lines) and their parts (the lines after one) of a 1.x session.
// A "model" line is both sessions' model column. So every JSON the adapter parses is the
// input's, read through both layouts.
func FuzzRead(f *testing.F) {
	for _, seed := range seeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		path := filepath.Join(t.TempDir(), "opencode.db")
		fill(t, path, input)
		sourcetest.Attempt(t, Spec.Open(path))
	})
}

// fill writes the database FuzzRead reads. It is written in one transaction and never
// synced: a database per input, built statement by statement with a sync after each, is
// most of what a run of the fuzzer would spend.
func fill(t *testing.T, path string, input []byte) {
	t.Helper()
	type row struct{ kind, data string }
	var rows []row
	model := sql.NullString{}
	for _, line := range strings.Split(string(input), "\n") {
		if line == "" {
			continue
		}
		kind, data, ok := strings.Cut(line, "\t")
		if !ok {
			kind, data = "", line
		}
		if kind == "model" {
			model = sql.NullString{String: data, Valid: true}
			continue
		}
		rows = append(rows, row{kind, data})
	}

	db, err := sql.Open("sqlite", kit.FileURI(path)+"?_pragma=journal_mode(MEMORY)&_pragma=synchronous(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	run := func(query string, args ...any) {
		if _, err := tx.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	run(schemaV1)
	run(schemaV2)
	run(schemaV23)
	const base = int64(1790000000000)
	at := func(i int) int64 { return base + 1 + int64(i) }
	forked := at(len(rows) / 2)

	run(`INSERT INTO session_v2 (id, slug, directory, title, version, model, time_created, time_updated) VALUES ('ses_v2', 's', '/w', 'v2', '2.3.0', ?, ?, ?)`, model, base, at(len(rows)))
	run(`INSERT INTO session_v2 (id, fork_session_id, slug, directory, title, version, model, time_created, time_updated) VALUES ('ses_fork', 'ses_v2', 'f', '/w', 'fork', '2.3.0', ?, ?, ?)`, model, forked, at(len(rows)))
	run(`INSERT INTO session (id, directory, title, slug, model, time_created, time_updated) VALUES ('ses_v1', '/w', 'v1', 'v1', ?, ?, ?)`, model, base, at(len(rows)))
	message := ""
	for i, r := range rows {
		n := strconv.Itoa(i)
		run(`INSERT INTO session_message VALUES (?, 'ses_v2', ?, ?, ?, ?, ?)`, "m"+n, r.kind, i, at(i), at(i), r.data)
		run(`INSERT INTO session_message VALUES (?, 'ses_fork', ?, ?, ?, ?, ?)`, "f"+n, r.kind, i, at(i), at(i), r.data)
		if r.kind == "message" {
			message = "msg" + n
			run(`INSERT INTO message VALUES (?, 'ses_v1', ?, ?, ?)`, message, at(i), at(i), r.data)
			continue
		}
		if message == "" {
			message = "msg"
			run(`INSERT INTO message VALUES ('msg', 'ses_v1', ?, ?, '{"role":"assistant"}')`, base, base)
		}
		run(`INSERT INTO part VALUES (?, ?, 'ses_v1', ?, ?, ?)`, "p"+n, message, at(i), at(i), r.data)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// seeds are the fixture sessions of the tests, written as FuzzRead's input: the 2.x
// session of every shape, the parent of the fork, and the 1.x session.
func seeds(t testing.TB) [][]byte {
	_, v2 := build(t, append([]string{schemaV2}, rowsV2...)...)
	_, fork := build(t, append([]string{schemaV2, schemaV23}, rowsFork...)...)
	_, v1 := build(t, append([]string{schemaV1}, rowsV1...)...)
	return [][]byte{
		lines(t, v2, `SELECT type, data FROM session_message WHERE session_id = 'ses_2' ORDER BY seq`),
		lines(t, fork, `SELECT 'model', model FROM session_v2 WHERE id = 'ses_parent'
UNION ALL SELECT type, data FROM (SELECT type, data FROM session_message WHERE session_id = 'ses_parent' ORDER BY seq)`),
		lines(t, v1, `SELECT kind, data FROM (
  SELECT 'message' AS kind, data, time_created AS t, id AS m, 0 AS o, '' AS p FROM message WHERE session_id = 'ses_live'
  UNION ALL SELECT 'part', p.data, m.time_created, m.id, 1, p.id FROM part p JOIN message m ON m.id = p.message_id WHERE m.session_id = 'ses_live')
ORDER BY t, m, o, p`),
	}
}

// lines runs a query of (type, data) and writes its rows as lines, each JSON on one line.
func lines(t testing.TB, path, query string) []byte {
	db, err := sql.Open("sqlite", kit.FileURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out bytes.Buffer
	for rows.Next() {
		var kind, data string
		if err := rows.Scan(&kind, &data); err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(data)) == nil {
			data = compact.String()
		}
		out.WriteString(kind + "\t" + data + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
