package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动（不用 cgo，交叉编译照旧）
)

// hermesSQLiteFinal 读取 ~/.hermes/state.db，对应原 Python 版的 _sqlite_final_message。
//
// Hermes 的 webhook 会话有时只把最终消息落在 state.db 里，jsonl 里什么都没有——
// 这时候会话记录没有 file，final 只能从 SQLite 取。
//
// 返回 nil 表示「这里也没有最终消息」（库不存在、查不到、或读取失败），由上层走兜底响应。
func hermesSQLiteFinal(mode, sessionID, status string) map[string]any {
	if mode != "hermes" || sessionID == "" {
		return nil
	}
	dbPath := filepath.Join(defaultHome(), ".hermes", "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}

	// 只读打开：不建 -wal/-shm、不改动别人的库；超时保护与 Python 版的 timeout=3 对齐
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return nil
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var messageCount sql.NullInt64
	var inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens, reasoningTokens sql.NullInt64
	var estimatedCost sql.NullFloat64

	// 注意：这里不查 token_count——Python 版的 SELECT 里带了它但从未使用，
	// 少查一列反而更抗 schema 差异
	row := db.QueryRow(
		`SELECT message_count, input_tokens, output_tokens, cache_read_tokens,
		        cache_write_tokens, reasoning_tokens, estimated_cost_usd
		 FROM sessions WHERE id = ?`,
		sessionID,
	)
	hasSession := true
	switch err := row.Scan(&messageCount, &inputTokens, &outputTokens, &cacheReadTokens,
		&cacheWriteTokens, &reasoningTokens, &estimatedCost); {
	case errors.Is(err, sql.ErrNoRows):
		hasSession = false
	case err != nil:
		warnHermesSQLite(sessionID, err)
		return nil
	}

	var id, content, finishReason, reasoning, timestamp sql.NullString
	row = db.QueryRow(
		`SELECT id, content, finish_reason, reasoning, timestamp
		 FROM messages
		 WHERE session_id = ?
		   AND role = 'assistant'
		   AND COALESCE(active, 1) = 1
		   AND finish_reason = 'stop'
		   AND COALESCE(content, '') <> ''
		 ORDER BY timestamp DESC, id DESC
		 LIMIT 1`,
		sessionID,
	)
	switch err := row.Scan(&id, &content, &finishReason, &reasoning, &timestamp); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		warnHermesSQLite(sessionID, err)
		return nil
	}

	// message_count 缺失或为 0 时回退成实际条数（与 Python 版一致）
	count := int64(0)
	if messageCount.Valid {
		count = messageCount.Int64
	}
	if count == 0 {
		var actual sql.NullInt64
		err := db.QueryRow(
			`SELECT COUNT(*) FROM messages WHERE session_id = ? AND COALESCE(active, 1) = 1`,
			sessionID,
		).Scan(&actual)
		if err != nil {
			warnHermesSQLite(sessionID, err)
			return nil
		}
		if actual.Valid {
			count = actual.Int64
		}
	}

	usage := map[string]any{}
	if hasSession {
		usage = map[string]any{
			"inputTokens":      nullIntOrZero(inputTokens),
			"outputTokens":     nullIntOrZero(outputTokens),
			"cacheReadTokens":  nullIntOrZero(cacheReadTokens),
			"cacheWriteTokens": nullIntOrZero(cacheWriteTokens),
			"reasoningTokens":  nullIntOrZero(reasoningTokens),
			"estimatedCostUsd": nullFloatOrZero(estimatedCost),
		}
	}

	return map[string]any{
		"status":       status,
		"isFinal":      true,
		"isProcessing": false,
		"messageCount": count,
		"source":       mode,
		"id":           nullStringOrNil(id),
		"timestamp":    nullStringOrNil(timestamp),
		"stopReason":   strOr(finishReason.String, "stop"),
		"text":         content.String,
		"thinking":     reasoning.String,
		"toolCalls":    []any{},
		"usage":        usage,
	}
}

func nullIntOrZero(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

func nullFloatOrZero(v sql.NullFloat64) float64 {
	if v.Valid {
		return v.Float64
	}
	return 0
}

// openHermesDB 只读打开 state.db：不建 -wal/-shm、不改动别人的库
func openHermesDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// hermesSQLiteList 从 state.db 的 sessions 表列出会话。
// 新版 Hermes 不再写 sessions.json / 每会话一个 jsonl，会话全部落在 SQLite 里；
// skip 里登记的 sessionId（sessions.json 里已有的）跳过，避免双重列出。
func hermesSQLiteList(dbPath, mode string, skip map[string]bool) []record {
	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite("list", err)
		return nil
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT s.id, COALESCE(NULLIF(s.session_key, ''), s.id),
		       s.display_name, s.source, s.model,
		       s.input_tokens, s.output_tokens, s.estimated_cost_usd,
		       s.started_at, s.ended_at,
		       (SELECT MAX(m.timestamp) FROM messages m
		         WHERE m.session_id = s.id AND COALESCE(m.active, 1) = 1)
		FROM sessions s`)
	if err != nil {
		warnHermesSQLite("list", err)
		return nil
	}
	defer rows.Close()

	out := []record{}
	for rows.Next() {
		var sid, key, displayName, platform, model sql.NullString
		var inputTokens, outputTokens sql.NullInt64
		var cost sql.NullFloat64
		var startedAt, endedAt, lastMsg sql.NullFloat64
		if err := rows.Scan(&sid, &key, &displayName, &platform, &model,
			&inputTokens, &outputTokens, &cost, &startedAt, &endedAt, &lastMsg); err != nil {
			warnHermesSQLite("list", err)
			return out
		}
		if !sid.Valid || sid.String == "" || skip[sid.String] {
			continue
		}

		// updatedAt：最后一条消息时间 → 会话结束时间 → 开始时间
		updated := lastMsg
		if !updated.Valid {
			updated = endedAt
		}
		if !updated.Valid {
			updated = startedAt
		}
		updatedAt, sortKey := "", ""
		if updated.Valid {
			if _, iso, ok := utcFromSeconds(updated.Float64); ok {
				updatedAt, sortKey = iso, iso
			}
		}
		createdAt := ""
		if startedAt.Valid {
			if _, iso, ok := utcFromSeconds(startedAt.Float64); ok {
				createdAt = iso
			}
		}
		totalTokens := float64(nullIntOrZero(inputTokens)) + float64(nullIntOrZero(outputTokens))

		keyStr := key.String
		out = append(out, record{
			fields: map[string]any{
				"source":           mode,
				"key":              keyStr,
				"shortKey":         keyStr,
				"sessionId":        sid.String,
				"file":             nil,
				"hasFile":          false,
				"status":           "done",
				"updatedAt":        updatedAt,
				"createdAt":        createdAt,
				"displayName":      displayName.String,
				"platform":         platform.String,
				"model":            model.String,
				"totalTokens":      totalTokens,
				"estimatedCostUsd": nullFloatOrZero(cost),
			},
			sortKey: sortKey,
		})
	}
	return out
}

// hermesSQLiteMessages 读没有 jsonl 的会话的消息（state.db 里的 user/assistant 行，
// 与 jsonl 路径同构：assistant 的 reasoning 作为 thinking；时间戳是 epoch 秒 → UTC ISO）
func hermesSQLiteMessages(dbPath, sessionID string, limit int) []map[string]any {
	out := []map[string]any{}
	if limit <= 0 {
		return out
	}
	db, err := openHermesDB(dbPath)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return out
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT id, role, content, reasoning, timestamp
		FROM messages
		WHERE session_id = ?
		  AND COALESCE(active, 1) = 1
		  AND role IN ('user', 'assistant')
		ORDER BY timestamp ASC, id ASC
		LIMIT ?`, sessionID, limit)
	if err != nil {
		warnHermesSQLite(sessionID, err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var id, role, content, reasoning, timestamp any
		if err := rows.Scan(&id, &role, &content, &reasoning, &timestamp); err != nil {
			warnHermesSQLite(sessionID, err)
			return out
		}
		parts := []map[string]any{}
		if r, ok := reasoning.(string); ok && r != "" {
			parts = append(parts, map[string]any{"type": "thinking", "content": truncate(r, 1000, "...[truncated]")})
		}
		if c, ok := content.(string); ok && c != "" {
			parts = append(parts, map[string]any{"type": "text", "content": c})
		}
		out = append(out, map[string]any{
			"id":        sqliteValueString(id),
			"role":      sqliteValueString(role),
			"timestamp": sqliteTimeString(timestamp),
			"content":   parts,
		})
	}
	return out
}

// sqliteValueString 把 SQLite 动态类型的值收敛成字符串（id/role 实际是 TEXT 或 INTEGER）
func sqliteValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// sqliteTimeString 兼容时间戳的几种存储形态：REAL epoch 秒 → UTC ISO；
// TEXT（ISO 字符串）原样——数字字符串（TEXT 亲和性存进去的）也按 epoch 处理
func sqliteTimeString(v any) string {
	if s, ok := v.(string); ok {
		if sec, err := strconv.ParseFloat(s, 64); err == nil {
			if _, iso, ok := utcFromSeconds(sec); ok {
				return iso
			}
		}
		return s
	}
	if sec, ok := toFloat(v); ok {
		if _, iso, ok := utcFromSeconds(sec); ok {
			return iso
		}
	}
	return sqliteValueString(v)
}

// nullStringOrNil 对应 Python 里「字段原样返回」：NULL 就是 JSON null
func nullStringOrNil(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

func warnHermesSQLite(sessionID string, err error) {
	fmt.Fprintf(os.Stderr, "[WARN] 读取 Hermes SQLite 最终消息失败 %s: %v\n", sessionID, err)
}
