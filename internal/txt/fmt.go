package txt

import (
	"math"
	"strconv"
	"time"
)

func itoa(n int) string { return strconv.Itoa(n) }

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 1, 64)
}

// Tokens renders a token count the way a line reads it: 940, 18.2k, 3.4M, 1.2B.
func Tokens(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	units := [...]string{"k", "M", "B"}
	v, i := float64(n)/1e3, 0
	// What would round up to a whole unit is written in that unit: 1.0M, not 1000.0k
	for i < len(units)-1 && math.Round(v*10) >= 10_000 {
		v, i = v/1e3, i+1
	}
	return trimFloat(v) + units[i]
}

// Duration renders an elapsed time compactly: 850ms, 12s, 4m07s, 2h13m, 3d4h.
func Duration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	case d < time.Minute:
		return strconv.FormatInt(int64(d.Seconds()), 10) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m" + pad2(int(d.Seconds())%60) + "s"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h" + pad2(int(d.Minutes())%60) + "m"
	}
	return strconv.Itoa(int(d.Hours())/24) + "d" + strconv.Itoa(int(d.Hours())%24) + "h"
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// Ago renders how long ago t was, relative to now: "just now", "5m ago", "3h ago", "2d ago",
// and a date once it is more than a month.
func Ago(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	case d < 30*24*time.Hour:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
	return t.Local().Format("2006-01-02")
}
