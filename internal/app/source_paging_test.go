package app

import "testing"

func TestMessageSinkOffsetPaging(t *testing.T) {
	messages := make([]map[string]any, 10)
	for i := range messages {
		messages[i] = map[string]any{"id": string(rune('a' + i))}
	}

	page := func(q messageQuery) []map[string]any {
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

	if got := ids(page(messageQuery{limit: 3, fromEnd: true})); got != "hij" {
		t.Fatalf("latest page = %q, want hij", got)
	}
	if got := ids(page(messageQuery{limit: 3, fromEnd: true, offset: 3})); got != "efg" {
		t.Fatalf("older page = %q, want efg", got)
	}
	if got := ids(page(messageQuery{limit: 3, offset: 3})); got != "def" {
		t.Fatalf("newer page = %q, want def", got)
	}
}
