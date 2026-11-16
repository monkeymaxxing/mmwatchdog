package testhelpers

import (
	"sync"
	"time"
	"unsafe"
)

// Clock is a clock for the tests of MMWatchdog's waits. Its wall clock can jump, as
// the system clock of a node does when a time service steps it, while its
// monotonic clock runs on. MMWatchdog times every wait on Go's monotonic clock
// reading, so a test gives Clock.Now to the daemon as its clock, makes the
// wall clock jump, and checks that the jump holds back no wait and ends none
// early. Time on the clock passes only when the test calls Pass.
//
// Each time that Now returns carries a wall clock reading and a monotonic
// clock reading, as a time from time.Now does. Go's public API cannot build
// such a time with a wall reading of its own: Add moves both readings, and
// every function that changes only the wall reading, such as Round,
// Truncate, AddDate, In, UTC, and decoding, drops the monotonic one. So Now
// writes the wall reading into the time with package unsafe, in the layout
// that time.Time has in Go 1.26. TestClock fails when Go changes that
// layout. Only tests use this clock; MMWatchdog's own code never uses unsafe.
type Clock struct {
	mu sync.Mutex
	// at is the clock's time without its jumps, with both readings, and
	// jumped is how far its wall clock has jumped in all.
	at     time.Time
	jumped time.Duration
}

// NewClock returns a clock that stands at the present.
func NewClock() *Clock { return &Clock{at: time.Now()} }

// Pass lets d pass on the clock: both readings move by d.
func (c *Clock) Pass(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// Jump steps the wall clock by d, forward or backward, as a time service
// does. The monotonic clock does not move.
func (c *Clock) Jump(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jumped += d
}

// startOfTheWallReading is where the wall reading of a time with a monotonic
// reading counts its seconds from.
var startOfTheWallReading = time.Date(1885, time.January, 1, 0, 0, 0, 0, time.UTC)

// Now is the clock's time: its monotonic reading, and its wall reading with
// every jump.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.at
	wall := c.at.Round(0).Add(c.jumped)
	// A time.Time is {wall uint64; ext int64; loc *Location}. With a
	// monotonic reading, ext holds that reading, and wall holds a flag in
	// its top bit, the seconds since 1885 in the next 33 bits, and the
	// nanoseconds in its lowest 30 bits.
	layout := (*struct {
		wall uint64
		ext  int64
		loc  *time.Location
	})(unsafe.Pointer(&t))
	layout.wall = 1<<63 | uint64(wall.Unix()-startOfTheWallReading.Unix())<<30 | uint64(wall.Nanosecond())
	return t
}
