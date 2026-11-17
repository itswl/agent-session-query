package dsh

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime/debug"
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
		// go test -fuzz gives no one input a time limit: a read that never ends stalls its
		// worker until -fuzztime runs out, the run ends as a pass with nothing saved, and a
		// check made after the read returns is never reached. The watchdog panics instead. A
		// panic on any goroutine ends the worker's process, the fuzzer saves the input it was
		// on as a crasher, and with every goroutine's stack printed the one the read is stuck
		// in is among them.
		watchdog := time.AfterFunc(hang, func() {
			debug.SetTraceback("all")
			panic(fmt.Sprintf("reading a %d-byte transcript has not ended in %s", len(data), hang))
		})
		defer watchdog.Stop()
		sourcetest.Attempt(t, Spec.Open(root))
	})
}

// hang is how long reading one input may take before it is taken for a read that will not
// end. A transcript of the size a fuzzer writes reads in milliseconds — the seeds, all of
// them, in about 0.04 s under -race — so a read still going after this is stuck, not slow.
const hang = 10 * time.Second
