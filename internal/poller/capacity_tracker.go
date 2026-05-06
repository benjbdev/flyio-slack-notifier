package poller

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
)

// capacityTracker watches per-app counts of running machines and
// emits when the running count drops below the high-water-mark
// observed since startup. Surfaces a min_machines_running shortfall
// (one of two machines vanished after a deploy and never came back)
// as an immediate alert instead of waiting for the next digest.
//
// In-memory only: a restart re-seeds HWM on the bootstrap pass so we
// never alert against a fictional pre-startup expectation.
type capacityTracker struct {
	mu       sync.Mutex
	hwm      map[string]int
	degraded map[string]bool
}

func newCapacityTracker() *capacityTracker {
	return &capacityTracker{
		hwm:      map[string]int{},
		degraded: map[string]bool{},
	}
}

// seed updates the HWM without ever returning an event. Called during
// the bootstrap pass so the first real observation isn't compared
// against zero.
func (c *capacityTracker) seed(app string, running int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if running > c.hwm[app] {
		c.hwm[app] = running
	}
}

// observe records the current running count and returns a degradation
// event when running first drops below HWM, a recovery event when it
// climbs back, or (zero, false) when nothing newly transitioned.
func (c *capacityTracker) observe(app string, running int, now time.Time) (event.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if running > c.hwm[app] {
		c.hwm[app] = running
	}
	hwm := c.hwm[app]

	if hwm == 0 {
		return event.Event{}, false
	}

	if running < hwm && !c.degraded[app] {
		c.degraded[app] = true
		return event.Event{
			Kind:      event.KindCapacityDegraded,
			Severity:  event.SeverityCritical,
			App:       app,
			Timestamp: now,
			Title:     fmt.Sprintf("%s capacity degraded — %d / %d running", app, running, hwm),
			Detail: fmt.Sprintf(
				"%d of %d expected machines are running (high-water-mark since notifier started). If the missing machine doesn't come back, you've lost redundancy.",
				running, hwm,
			),
			Fields: map[string]string{
				"app":      app,
				"running":  strconv.Itoa(running),
				"expected": strconv.Itoa(hwm),
			},
		}, true
	}
	if running >= hwm && c.degraded[app] {
		c.degraded[app] = false
		return event.Event{
			Kind:      event.KindCapacityRestored,
			Severity:  event.SeverityInfo,
			App:       app,
			Timestamp: now,
			Title:     fmt.Sprintf("%s capacity restored (%d running)", app, running),
			Fields: map[string]string{
				"app":     app,
				"running": strconv.Itoa(running),
			},
		}, true
	}
	return event.Event{}, false
}
