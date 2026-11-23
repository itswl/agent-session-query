package app

import (
	"context"
	"os"
	"time"

	"github.com/itswl/agent-session-query/internal/index"
)

// SetStat stands in for how a file is looked at, and SetLookWait for how long a look is
// waited for, in a test; each returns how to put it back.
func SetStat(f func(string) (os.FileInfo, error)) func() {
	old := osStat
	osStat = f
	return func() { osStat = old }
}

func SetLookWait(d time.Duration) func() {
	old := lookWait
	lookWait = d
	return func() { lookWait = old }
}

// SetHeldPictures stands in for how much of the pictures read lately is kept, in a test.
func SetHeldPictures(n int) func() {
	old := heldPictures
	heldPictures = n
	return func() { heldPictures = old }
}

// BusyPictures has a turn's pictures read by another request, which keeps none of them,
// until release is called.
func BusyPictures(a *App, s *index.Session, turn int) (release func()) {
	started, gate := make(chan struct{}), make(chan struct{})
	go a.pictures.once(context.Background(), pictureTurn(s, turn), func(context.Context) error {
		close(started)
		<-gate
		return nil
	})
	<-started
	return func() { close(gate) }
}
