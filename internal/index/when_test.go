package index

import (
	"testing"
	"time"
)

// A turn's time is laid into each hour of the week it ran through, on the local clock: one
// that runs from Sunday 23:40 to Monday 00:20 is twenty minutes of Sunday's last hour and
// twenty of Monday's first.
func TestSpreadLaysATurnIntoItsHours(t *testing.T) {
	var grid [7][24]int64
	sunday := time.Date(2026, 11, 22, 23, 40, 0, 0, time.Local)
	spread(&grid, sunday, sunday.Add(40*time.Minute))
	spread(&grid, sunday.Add(-6*time.Hour), sunday.Add(-6*time.Hour)) // a turn with no time takes none
	tuesday := time.Date(2026, 11, 24, 9, 15, 0, 0, time.Local)
	spread(&grid, tuesday, tuesday.Add(2*time.Hour))
	const minute = int64(time.Minute / time.Millisecond)
	want := map[[2]int]int64{
		{6, 23}: 20 * minute, // Sunday, the last row
		{0, 0}:  20 * minute, // Monday, the first
		{1, 9}:  45 * minute,
		{1, 10}: 60 * minute,
		{1, 11}: 15 * minute,
	}
	var sum int64
	for d, row := range grid {
		for h, ms := range row {
			sum += ms
			if ms != want[[2]int{d, h}] {
				t.Errorf("day %d hour %d = %dm, want %dm", d, h, ms/minute, want[[2]int{d, h}]/minute)
			}
		}
	}
	if sum != 160*minute {
		t.Errorf("the grid holds %dm, want the 160 the turns took", sum/minute)
	}
}
