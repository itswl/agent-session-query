package source

import (
	"database/sql"
	"os"
	"path/filepath"
)

// SessionStorage is one session's share of the disk, for the storage report: the bytes
// of the session's own transcript and the files that belong to it, the project the list
// groups it under, and the time its age is read from.
type SessionStorage struct {
	SessionID string
	Project   string
	Bytes     int64
	UpdatedAt string
}

// StorageReporter sizes what a source keeps on disk, per session. The overhead is what
// the source keeps besides any session — indexes, locks, prompt history, the free pages
// of a database — so the two together account for the whole data directory.
type StorageReporter interface {
	Storage() (sessions []SessionStorage, overhead int64)
}

// sessionStorageFromRecords sizes the file-shaped sources: each record owns the paths
// prefixOf names (its transcript, and a directory when the session keeps one — Claude
// Code's subagents, Grok's per-session files), everything else under the roots is
// overhead.
//
// Exact files are claimed before directory prefixes, so a subagent's own transcript
// belongs to the subagent's record rather than to the session whose directory it sits in.
func sessionStorageFromRecords(roots []string, recs []Record, prefixOf func(Record) []string) ([]SessionStorage, int64) {
	// every file under the roots, by path; claimed entries are deleted as they are
	// attributed, so nothing is counted twice
	sizes := map[string]int64{}
	total := int64(0)
	for _, root := range roots {
		if !fileExists(root) {
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil // an unreadable subtree is skipped, not fatal to a report
			}
			sizes[path] = info.Size()
			total += info.Size()
			return nil
		})
	}

	out := []SessionStorage{}
	attributed := int64(0)
	rows := make([]SessionStorage, len(recs))
	for i, rec := range recs {
		rows[i] = SessionStorage{
			SessionID: rec.SessionID,
			Project:   rec.ProjectName(),
			UpdatedAt: rec.UpdatedAt,
		}
	}
	claimFile := func(i int, path string) {
		if size, ok := sizes[path]; ok {
			rows[i].Bytes += size
			attributed += size
			delete(sizes, path)
		}
	}
	claimDir := func(i int, dir string) {
		dir = dir + string(filepath.Separator)
		for path, size := range sizes {
			if len(path) > len(dir) && path[:len(dir)] == dir {
				rows[i].Bytes += size
				attributed += size
				delete(sizes, path)
			}
		}
	}
	for i, rec := range recs {
		for _, prefix := range prefixOf(rec) {
			claimFile(i, prefix)
		}
	}
	for i, rec := range recs {
		for _, prefix := range prefixOf(rec) {
			if prefix == "" {
				continue
			}
			claimDir(i, prefix)
		}
	}
	out = append(out, rows...)
	overhead := total - attributed
	if overhead < 0 {
		overhead = 0
	}
	return out, overhead
}

// sqliteFileSize is what a database and its sidecars take on disk
func sqliteFileSize(dbPath string) int64 {
	total := int64(0)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if info, err := os.Stat(dbPath + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Storage sizes the sessions pi keeps: one jsonl per session, nothing else beside them.
func (s *PiSource) Storage() ([]SessionStorage, int64) {
	return sessionStorageFromRecords([]string{s.root}, s.List(), func(rec Record) []string {
		return []string{rec.File}
	})
}

// Storage sizes the sessions Claude Code keeps: the session's jsonl and the directory of
// the same name, where its subagents' transcripts live — a subagent's own record claims
// its transcript first, so nesting never double-counts.
func (s *ClaudeCodeSource) Storage() ([]SessionStorage, int64) {
	return sessionStorageFromRecords([]string{s.root}, s.List(), func(rec Record) []string {
		owned := []string{rec.File}
		if ext := filepath.Ext(rec.File); ext != "" {
			owned = append(owned, rec.File[:len(rec.File)-len(ext)])
		}
		return owned
	})
}

// Storage sizes the sessions Codex keeps: one rollout per session, the dated tree and
// the archived flat shape both.
func (s *CodexSource) Storage() ([]SessionStorage, int64) {
	roots := []string{s.root}
	if archived := s.archivedRoot(); archived != "" {
		roots = append(roots, archived)
	}
	return sessionStorageFromRecords(roots, s.List(), func(rec Record) []string {
		return []string{rec.File}
	})
}

// Storage sizes the sessions Gemini CLI keeps: one session jsonl per chat.
func (s *GeminiSource) Storage() ([]SessionStorage, int64) {
	return sessionStorageFromRecords([]string{s.root}, s.List(), func(rec Record) []string {
		return []string{rec.File}
	})
}

// Storage sizes the sessions Grok keeps: a session is a directory, everything in it is
// the session's; the prompt history and the search index beside the groups are overhead.
func (s *GrokSource) Storage() ([]SessionStorage, int64) {
	roots := []string{s.root}
	if archived := s.archivedRoot(); archived != "" {
		roots = append(roots, archived)
	}
	return sessionStorageFromRecords(roots, s.List(), func(rec Record) []string {
		return []string{filepath.Dir(rec.File)}
	})
}

// Usage answers the sessions' accounting from Hermes's database when the all-SQLite
// layout is the one in use; the jsonl layout answers through its finals instead.
func (s *JsonMapSource) Usage() []SessionUsage {
	if s.def.stateDB != "" && fileExists(s.def.stateDB) && !fileExists(s.def.sessionsJSON) {
		return hermesSQLiteUsage(s.def.stateDB)
	}
	return nil
}

// Storage sizes the sessions the json-map layout keeps: one jsonl per session, the
// index file beside them is overhead. Hermes's newer all-SQLite layout keeps no jsonl
// at all — its database is sized instead.
func (s *JsonMapSource) Storage() ([]SessionStorage, int64) {
	if s.def.stateDB != "" && fileExists(s.def.stateDB) && !fileExists(s.def.sessionsJSON) {
		return hermesSQLiteStorage(s.def.stateDB)
	}
	return sessionStorageFromRecords([]string{s.def.sessionsDir}, s.List(), func(rec Record) []string {
		return []string{rec.File}
	})
}

// Storage sizes Hermes's state.db per session: the bytes of a session's message rows
// are its share; the file's remainder — the sessions table, indexes, free pages — is
// overhead. The pre-SQLite jsonl layout is sized by the json-map source instead.
func hermesSQLiteStorage(dbPath string) (out []SessionStorage, overhead int64) {
	attributed := int64(0)
	err := readHermesDB(dbPath, func(db *sql.DB) error {
		rows, err := db.Query(`
			SELECT s.id, COALESCE(s.cwd, ''),
			       (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = s.id),
			       (SELECT COALESCE(SUM(COALESCE(LENGTH(m.content), 0) + COALESCE(LENGTH(m.reasoning), 0)), 0)
				  FROM messages m WHERE m.session_id = s.id)
			FROM sessions s`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sid, cwd sql.NullString
			var lastMsg, bytes sql.NullFloat64
			if err := rows.Scan(&sid, &cwd, &lastMsg, &bytes); err != nil {
				return err
			}
			if !sid.Valid || sid.String == "" {
				continue
			}
			updatedAt := ""
			if _, iso, ok := utcFromSeconds(lastMsg.Float64); ok {
				updatedAt = iso
			}
			attributed += int64(bytes.Float64)
			out = append(out, SessionStorage{
				SessionID: sid.String,
				Project:   cwd.String,
				Bytes:     int64(bytes.Float64),
				UpdatedAt: updatedAt,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0
	}
	overhead = sqliteFileSize(dbPath) - attributed
	if overhead < 0 {
		overhead = 0
	}
	return out, overhead
}

// Storage sizes the sessions OpenClaw keeps, one agent database at a time: each
// session's transcript_events bytes are its share, everything else in the databases is
// overhead.
func (s *OpenClawSource) Storage() ([]SessionStorage, int64) {
	out := []SessionStorage{}
	total := int64(0)
	attributed := int64(0)
	for _, dbPath := range s.dbPaths {
		total += sqliteFileSize(dbPath)
		db, err := s.open(dbPath)
		if err != nil {
			continue
		}
		rows, err := db.Query(`
			SELECT w.session_id,
			       (SELECT COALESCE(json_extract(te.event_json, '$.cwd'), '')
				  FROM transcript_events te
				 WHERE te.session_id = w.session_id
				 ORDER BY te.seq LIMIT 1),
			       COALESCE(w.transcript_updated_at, w.updated_at),
			       (SELECT COALESCE(SUM(LENGTH(te.event_json)), 0)
				  FROM transcript_events te WHERE te.session_id = w.session_id)
			FROM session_windows w`)
		if err != nil {
			db.Close()
			continue
		}
		for rows.Next() {
			var sid, cwd sql.NullString
			var updated, bytes sql.NullInt64
			if err := rows.Scan(&sid, &cwd, &updated, &bytes); err != nil {
				break
			}
			if !sid.Valid || sid.String == "" || !bytes.Valid {
				continue
			}
			attributed += bytes.Int64
			out = append(out, SessionStorage{
				SessionID: sid.String,
				Project:   cwd.String,
				Bytes:     bytes.Int64,
				UpdatedAt: millisToISO(updated),
			})
		}
		rows.Close()
		db.Close()
	}
	overhead := total - attributed
	if overhead < 0 {
		overhead = 0
	}
	return out, overhead
}

// owns are its share, both layouts read, and the file's remainder — indexes, free pages,
// the WAL — is overhead.
func (s *OpenCodeSource) Storage() (out []SessionStorage, overhead int64) {
	db, err := s.open()
	if err != nil {
		return nil, 0
	}
	defer db.Close()

	byID := map[string]*SessionStorage{}
	add := func(id, directory string, updated sql.NullInt64, bytes sql.NullInt64) {
		if id == "" || !bytes.Valid {
			return
		}
		row, ok := byID[id]
		if !ok {
			row = &SessionStorage{SessionID: id, Project: directory, UpdatedAt: millisToISO(updated)}
			byID[id] = row
		}
		row.Bytes += bytes.Int64
	}
	scan := func(rows *sql.Rows, withParts bool) {
		for rows.Next() {
			var id, directory sql.NullString
			var updated, msgBytes sql.NullInt64
			var partBytes sql.NullInt64
			if withParts {
				if rows.Scan(&id, &directory, &updated, &msgBytes, &partBytes) == nil {
					add(id.String, directory.String, updated, msgBytes)
					add(id.String, directory.String, updated, partBytes)
				}
				continue
			}
			if rows.Scan(&id, &directory, &updated, &msgBytes) == nil {
				add(id.String, directory.String, updated, msgBytes)
			}
		}
		rows.Close()
	}
	if openCodeHasV2(db) {
		if rows, err := db.Query(`
			SELECT s.id, s.directory, s.time_updated,
			       (SELECT COALESCE(SUM(LENGTH(m.data)), 0) FROM session_message m WHERE m.session_id = s.id)
			FROM session_v2 s`); err == nil {
			scan(rows, false)
		}
		if openCodeHasTable(db, "session") {
			// the V1 sessions that never migrated are history too
			if rows, err := db.Query(`
				SELECT s.id, s.directory, s.time_updated,
				       (SELECT COALESCE(SUM(LENGTH(m.data)), 0) FROM message m WHERE m.session_id = s.id
					        AND NOT EXISTS (SELECT 1 FROM session_v2 v WHERE v.id = m.session_id)),
				       (SELECT COALESCE(SUM(LENGTH(p.data)), 0) FROM part p WHERE p.session_id = s.id
					        AND NOT EXISTS (SELECT 1 FROM session_v2 v WHERE v.id = p.session_id))
				FROM session s`); err == nil {
				scan(rows, true)
			}
		}
	} else if rows, err := db.Query(`
		SELECT s.id, s.directory, s.time_updated,
		       (SELECT COALESCE(SUM(LENGTH(m.data)), 0) FROM message m WHERE m.session_id = s.id),
		       (SELECT COALESCE(SUM(LENGTH(p.data)), 0) FROM part p WHERE p.session_id = s.id)
		FROM session s`); err == nil {
		scan(rows, true)
	}

	attributed := int64(0)
	for _, row := range byID {
		attributed += row.Bytes
		out = append(out, *row)
	}
	overhead = sqliteFileSize(s.dbPath) - attributed
	if overhead < 0 {
		overhead = 0
	}
	return out, overhead
}
