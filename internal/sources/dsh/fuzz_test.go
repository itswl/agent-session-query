package dsh

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/itswl/agent-session-query/internal/sources/sourcetest"
)

// zstdMagic opens every zstd frame.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// FuzzRead: whatever a session directory's transcript holds — cut short, damaged, of a
// format the harness never wrote — reading it ends in a session or an error, never a
// panic, and what it reads holds what every session must. The harness stores a
// generation plain or as a chain of zstd frames, and an input is stored the way it looks:
// as frames when it opens with a frame's magic number, plain otherwise. The JSON seeds
// reach the rows, the framed one the decoding of a chain.
func FuzzRead(f *testing.F) {
	f.Add(encode(f, transcriptRows()))
	f.Add(encode(f, currentRows()))
	f.Add(append(frames(f, encode(f, transcriptRows()), 97), halfFrame(f)...))
	f.Add(encode(f, []M{
		headerRow("dsh-ctx", "/w"),
		{"type": "user/message", "time": ms(0), "data": M{"id": "u0", "source": M{"kind": "context"}, "content": text("Project instructions: be brief.")}},
		userRow("u1", time.Second, "a bare string is one text block"),
		assistantRow("a1", 2*time.Second, M{"input_tokens": 10, "output_tokens": 3},
			M{"type": "reasoning", "text": "thinking it over"},
			M{"type": "tool-call", "id": "c9", "name": "bash", "arguments": M{"command": "ls"}}),
		{"type": "job/settled", "time": ms(3 * time.Second), "data": M{"toolCall": M{"callId": "c9"},
			"startedAt": ms(2 * time.Second), "finishedAt": ms(3 * time.Second), "detail": "exit code: 2"}},
		resultRow("r1", "c9", 3*time.Second, false, "ls: missing"),
	}))
	f.Fuzz(func(t *testing.T, data []byte) {
		name := "session.v1.jsonl"
		if bytes.HasPrefix(data, zstdMagic) {
			name += ".zstd"
		}
		root := t.TempDir()
		put(t, filepath.Join(sessionDir(root), name), data)
		start := time.Now()
		sourcetest.Attempt(t, Spec.Open(root))
		// A transcript of the size a fuzzer writes reads in a millisecond: one that takes a
		// second is a read that has stopped making headway, and as much a failure as a panic
		if took := time.Since(start); took > time.Second {
			t.Errorf("reading took %s", took)
		}
	})
}
