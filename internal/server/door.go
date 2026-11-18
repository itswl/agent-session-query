package server

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What the guards did, written down.
//
// A caller the guards turn away is told why, and the owner is to hear of it as well: a
// token being guessed at, a caller shut out for it, a tunnel pointed at a server without
// a token, a sign-in. Each is a line in the log, naming the caller as the limiter names it
// (see caller) and never the token it brought or the cookie it was given, and a count
// over the last day in /api/health, behind the same guards as the rest of the record.
//
// The log is not to be filled by the callers it reports on. A caller's first line of a
// kind in a minute is written and the rest of that minute only counted; past linesMax
// lines in a minute nothing more is written; and once the minute is over, one line says
// how many of each kind were not. A flood costs the log linesMax+1 lines a minute.

// event is a kind of thing the guards did.
type event int

const (
	wrongToken event = iota // a token offered, in a header or at sign-in, that is not the token
	refused                 // a caller turned away whatever it brought: past an allowance, or through a proxy to a server without a token
	signedIn                // the token traded for the cookie
	eventKinds
)

// eventNames are what a line that sums up a minute calls each kind.
var eventNames = [eventKinds]string{"wrong token", "refusal", "sign-in"}

const (
	linesMax   = 10          // lines written in a minute, of every kind and caller together
	noteWindow = time.Minute // a caller's line of a kind is written once in it
	countedFor = 24          // hours the counts cover
)

// door writes down what the guards did and counts it. It needs no goroutine of its own:
// a minute that is over is summed up by the next thing noted, or by flush, which the
// watcher calls every couple of seconds.
type door struct {
	mu    sync.Mutex
	out   io.Writer // nil writes nothing, and still counts
	began time.Time // when the server started: there are no counts from before

	minute time.Time       // when the minute the lines are being counted in began; zero when none has
	lines  int             // written in it
	said   map[string]bool // the kinds and callers a line was written for in it
	unsaid [eventKinds]int // what was not written in it, of each kind

	hours [countedFor]tally
}

// tally is one hour's counts.
type tally struct {
	hour int64 // hours since the epoch
	n    [eventKinds]int
}

// guardCounts are what the guards did over the last day: from Since, the start of the
// hour 23 hours before this one, or the server's start if that is later.
type guardCounts struct {
	Since       time.Time `json:"since"`
	WrongTokens int       `json:"wrongTokens"`
	Refused     int       `json:"refused"`
	SignIns     int       `json:"signIns"`
}

// note counts one thing the guards did, and writes it down unless the minute has had its
// lines (see door). caller is the limiter's name for the caller; why, said after it.
func (d *door) note(now time.Time, ev event, caller, why string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	hour := now.Unix() / 3600
	t := &d.hours[hour%countedFor]
	if t.hour != hour {
		*t = tally{hour: hour}
	}
	t.n[ev]++

	d.roll(now)
	if d.minute.IsZero() {
		d.minute = now
	}
	key := strconv.Itoa(int(ev)) + " " + caller
	if d.said[key] || d.lines >= linesMax {
		d.unsaid[ev]++
		return
	}
	if d.said == nil {
		d.said = map[string]bool{}
	}
	d.said[key] = true
	d.lines++
	switch ev {
	case wrongToken:
		d.write(now, fmt.Sprintf("a wrong token from %q, %s", caller, why))
	case refused:
		d.write(now, fmt.Sprintf("refused %q: %s", caller, why))
	case signedIn:
		d.write(now, fmt.Sprintf("signed in: %q", caller))
	}
}

// flush sums up the minute once it is over, if nothing noted since has.
func (d *door) flush(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.roll(now)
}

// roll ends the minute once it is over, saying what it did not write.
func (d *door) roll(now time.Time) {
	if d.minute.IsZero() || now.Sub(d.minute) < noteWindow {
		return
	}
	var held []string
	for ev, n := range d.unsaid {
		if n > 0 {
			held = append(held, plural(n, eventNames[ev]))
		}
	}
	if len(held) > 0 {
		d.write(now, fmt.Sprintf("the minute from %s held more than was written: %s", d.minute.Format(time.TimeOnly), strings.Join(held, ", ")))
	}
	d.minute, d.lines, d.unsaid = time.Time{}, 0, [eventKinds]int{}
	clear(d.said)
}

func (d *door) write(now time.Time, line string) {
	if d.out != nil {
		fmt.Fprintf(d.out, "%s %s\n", now.Format(time.RFC3339), line)
	}
}

// counts are the hours of the last day, added up.
func (d *door) counts(now time.Time) guardCounts {
	d.mu.Lock()
	defer d.mu.Unlock()
	hour := now.Unix() / 3600
	c := guardCounts{Since: time.Unix((hour-countedFor+1)*3600, 0)}
	if d.began.After(c.Since) {
		c.Since = d.began
	}
	for _, t := range d.hours {
		if t.hour <= hour && hour-t.hour < countedFor {
			c.WrongTokens += t.n[wrongToken]
			c.Refused += t.n[refused]
			c.SignIns += t.n[signedIn]
		}
	}
	return c
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
