// Package kit is what adapters share: reading lines, times and loosely-typed JSON,
// fingerprinting files, opening another program's SQLite database read-only.
package kit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// MaxLine caps a single line. A line longer than this ends the scan: nothing after it in
// the file is read, which is why Lines reports it instead of skipping silently.
const MaxLine = 256 << 20

const (
	bufSteady = 64 << 10 // a scan's smallest buffer, and the pool's usual resident
	bufStart  = 1 << 20  // a scan starts at the file's own size, up to this
)

var bufPool = sync.Pool{New: func() any { b := make([]byte, bufSteady); return &b }}

// ErrLineTooLong ends a scan at a line MaxLine cannot hold.
var ErrLineTooLong = errors.New("line longer than the scan cap")

// Lines calls fn with every non-empty line of a JSONL file, without its newline. The
// slice is only valid during the call. Returning false stops the scan. A path ending in
// .zst is read through a zstd decoder.
//
// The buffer is sized by the file and reused across scans, and a line longer than it is
// gathered once at its own length rather than by doubling.
func Lines(path string, fn func(line []byte) bool) error {
	return LinesFrom(path, Place{}, func(line []byte, _ Place) bool { return fn(line) })
}

// Place is where a line of a file begins: its offset, and the CRC-32C (Castagnoli) of the
// bytes before it. A read that goes on from a place can tell, by the sum, that the bytes
// before it are the ones it was found after (see Before).
type Place struct {
	At  int64  `json:"at"`
	Sum uint32 `json:"sum"`
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// LinesFrom is Lines from a place in the file on, telling fn where each line begins. A
// compressed file has no places: it is read from its start, and each line's place is
// {-1, 0}.
func LinesFrom(path string, from Place, fn func(line []byte, at Place) bool) error {
	if strings.HasSuffix(path, ".zst") || strings.HasSuffix(path, ".zstd") {
		if from.At != 0 {
			return fmt.Errorf("%s: a compressed file is read from its start", path)
		}
		return zstdLines(path, func(line []byte) bool { return fn(line, Place{At: -1}) })
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	want := bufSteady
	if info, err := f.Stat(); err == nil {
		if size := info.Size() - from.At; size > bufSteady {
			want = int(min(size, bufStart))
		}
	}
	if from.At != 0 {
		if _, err := f.Seek(from.At, io.SeekStart); err != nil {
			return err
		}
	}
	bufp := bufPool.Get().(*[]byte)
	buf := *bufp
	if cap(buf) < want {
		buf = make([]byte, want)
	}
	buf = buf[:cap(buf)]
	defer func() {
		*bufp = buf
		bufPool.Put(bufp)
	}()

	r := &lineReader{f: f, buf: buf}
	place := from
	for {
		line, err := r.next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		// The next line begins after this one and its newline. The last line of a file may
		// have none, and then no line begins after it
		at := place
		place.At += int64(len(line)) + 1
		place.Sum = crc32.Update(crc32.Update(place.Sum, castagnoli, line), castagnoli, newline)
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if !fn(line, at) {
			return nil
		}
	}
}

var newline = []byte{'\n'}

// Before reports whether the bytes of the file before p are the ones p was found after:
// the file is at least that long, and their sum is p's.
func Before(path string, p Place) bool {
	if p.At <= 0 {
		return p.At == 0 && p.Sum == 0
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := crc32.New(castagnoli)
	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	n, err := io.CopyBuffer(h, io.LimitReader(f, p.At), (*bufp)[:cap(*bufp)])
	return err == nil && n == p.At && h.Sum32() == p.Sum
}

// lineReader walks a file line by line, keeping the bytes after a newline in its buffer.
type lineReader struct {
	f    *os.File
	buf  []byte
	r, w int // unconsumed bytes are buf[r:w]
	eof  bool
}

func (r *lineReader) next() ([]byte, error) {
	for {
		if i := bytes.IndexByte(r.buf[r.r:r.w], '\n'); i >= 0 {
			line := r.buf[r.r : r.r+i]
			r.r += i + 1
			return line, nil
		}
		// What is buffered belongs to one unfinished line: move it to the front, so a
		// full buffer means the line alone fills it
		if r.r > 0 {
			copy(r.buf, r.buf[r.r:r.w])
			r.w -= r.r
			r.r = 0
		}
		if r.w == len(r.buf) {
			return r.gather()
		}
		if r.eof {
			if r.w == 0 {
				return nil, io.EOF
			}
			line := r.buf[r.r:r.w]
			r.r = r.w
			return line, nil
		}
		n, err := r.f.Read(r.buf[r.w:])
		r.w += n
		if err != nil {
			if err == io.EOF {
				r.eof = true
				continue
			}
			return nil, err
		}
	}
}

// gather reads a line longer than the buffer at the size it turns out to be: its end is
// found by scanning ahead without keeping what is passed, then it is read back in one go.
func (r *lineReader) gather() ([]byte, error) {
	end, err := r.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	lineStart := end - int64(r.w)
	length := int64(r.w)
	chunk := make([]byte, bufSteady)
	for terminated := false; !terminated; {
		n, readErr := r.f.Read(chunk)
		if i := bytes.IndexByte(chunk[:n], '\n'); i >= 0 {
			length += int64(i)
			terminated = true
			continue
		}
		length += int64(n)
		if length > MaxLine {
			return nil, ErrLineTooLong
		}
		if readErr == io.EOF {
			r.eof = true
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	line := make([]byte, length)
	if _, err := r.f.Seek(lineStart, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r.f, line); err != nil {
		return nil, err
	}
	// Leave the file just past the newline, if there was one
	if _, err := r.f.Seek(lineStart+length+1, io.SeekStart); err != nil {
		return nil, err
	}
	r.w, r.r = 0, 0
	return line, nil
}

// zstdLines walks one zstd-compressed JSONL stream. A stream that ends mid-frame — a
// file still being written — yields the lines that were complete and no error.
func zstdLines(path string, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return err
	}
	defer dec.Close()

	reader := bufio.NewReaderSize(dec, bufSteady)
	line := make([]byte, 0, bufSteady)
	for {
		part, readErr := reader.ReadSlice('\n')
		if len(line)+len(part) > MaxLine {
			return fmt.Errorf("%s: %w", path, ErrLineTooLong)
		}
		line = append(line, part...)
		switch readErr {
		case nil:
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && !fn(trimmed) {
				return nil
			}
			line = line[:0]
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				fn(trimmed)
			}
			return nil
		default:
			if errors.Is(readErr, io.ErrUnexpectedEOF) {
				return nil // the tail of a file still being written
			}
			return fmt.Errorf("%s: %w", path, readErr)
		}
	}
}

// Objects calls fn with every line of a JSONL file that decodes to a JSON object; lines
// that do not are skipped.
func Objects(path string, fn func(obj map[string]any) bool) error {
	return Lines(path, func(line []byte) bool {
		var obj map[string]any
		if json.Unmarshal(line, &obj) == nil && obj != nil {
			return fn(obj)
		}
		return true
	})
}

// ReadJSON decodes a JSON file holding one object; nil when it is missing or is not one.
func ReadJSON(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return nil
	}
	return obj
}
