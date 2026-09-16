package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
