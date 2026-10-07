package source

import "testing"

func TestMessageSinkOffsetPaging(t *testing.T) {
	messages := make([]map[string]any, 10)
	for i := range messages {
		messages[i] = map[string]any{"id": string(rune('a' + i))}
	}

	page := func(q MessageQuery) []map[string]any {
		sink := newMessageSink(q)
		for _, message := range messages {
			sink.add(message)
		}
		return sink.result()
	}
	ids := func(got []map[string]any) string {
		out := make([]byte, len(got))
		for i, message := range got {
			out[i] = []byte(message["id"].(string))[0]
		}
		return string(out)
	}

	if got := ids(page(MessageQuery{Limit: 3, FromEnd: true})); got != "hij" {
		t.Fatalf("latest page = %q, want hij", got)
	}
	if got := ids(page(MessageQuery{Limit: 3, FromEnd: true, Offset: 3})); got != "efg" {
		t.Fatalf("older page = %q, want efg", got)
	}
	if got := ids(page(MessageQuery{Limit: 3, Offset: 3})); got != "def" {
		t.Fatalf("newer page = %q, want def", got)
	}
}

// TestMessageSinkDeepPage: a page whose limit+offset runs past the old 20000-message ring
// clamp must still come back exact. The clamp silently truncated the ring, so result()
// sliced a short buffer: a request the HTTP layer explicitly accepts (offset up to 20000,
// limit up to maxLimit) returned a short, misaligned or empty page with no error anywhere.
func TestMessageSinkDeepPage(t *testing.T) {
	const total = 25000
	messages := make([]map[string]any, total)
	for i := range messages {
		messages[i] = map[string]any{"id": i}
	}
	page := func(q MessageQuery) []map[string]any {
		sink := newMessageSink(q)
		for _, message := range messages {
			sink.add(message)
		}
		return sink.result()
	}

	// offset 19500 from the newest end, 1000 wide: messages ranked 19500..20499 from the
	// end, i.e. indices 4500..5499. The clamp returned 500 misaligned entries for this.
	got := page(MessageQuery{Limit: 1000, FromEnd: true, Offset: 19500})
	if len(got) != 1000 {
		t.Fatalf("deep page size = %d, want 1000", len(got))
	}
	if got[0]["id"] != 4500 || got[999]["id"] != 5499 {
		t.Fatalf("deep page spans %v..%v, want 4500..5499", got[0]["id"], got[999]["id"])
	}

	// The deepest offset the HTTP layer accepts is a real page, not an empty one
	deep := page(MessageQuery{Limit: 5, FromEnd: true, Offset: 20000})
	if len(deep) != 5 || deep[0]["id"] != 4995 || deep[4]["id"] != 4999 {
		t.Fatalf("offset=20000 -> %d items starting at %v, want 4995..4999", len(deep), deep[0]["id"])
	}

	// Past the session's start stays an empty page
	if out := page(MessageQuery{Limit: 10, FromEnd: true, Offset: total}); len(out) != 0 {
		t.Fatalf("offset past the start -> %d items, want none", len(out))
	}
}

// TestMessageSinkRolePaging: with a role set, the window and the offset count matching
// messages, not raw rows — the property get_messages' cursor is built on.
func TestMessageSinkRolePaging(t *testing.T) {
	messages := make([]map[string]any, 10)
	for i := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		messages[i] = map[string]any{"id": i, "role": role}
	}
	page := func(q MessageQuery) []map[string]any {
		sink := newMessageSink(q)
		for _, message := range messages {
			sink.add(message)
		}
		return sink.result()
	}
	ids := func(items []map[string]any) []int {
		out := make([]int, len(items))
		for i, m := range items {
			out[i] = m["id"].(int)
		}
		return out
	}

	got := ids(page(MessageQuery{Limit: 2, FromEnd: true, Role: "user"}))
	if len(got) != 2 || got[0] != 6 || got[1] != 8 {
		t.Fatalf("latest user messages = %v, want [6 8]", got)
	}
	got = ids(page(MessageQuery{Limit: 2, FromEnd: true, Role: "user", Offset: 1}))
	if len(got) != 2 || got[0] != 4 || got[1] != 6 {
		t.Fatalf("offset user messages = %v, want [4 6]", got)
	}
	got = ids(page(MessageQuery{Limit: 1, Role: "user", Offset: 2}))
	if len(got) != 1 || got[0] != 4 {
		t.Fatalf("ascending user messages = %v, want [4]", got)
	}
}
