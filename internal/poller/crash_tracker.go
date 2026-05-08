package poller

import (
	"fmt"
	"sync"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
)

const (
	defaultCrashLoopThreshold = 3
	defaultCrashLoopWindow    = 10 * time.Minute
	defaultCrashLoopCooldown  = 10 * time.Minute
)

// crashTracker counts OOMs and crashes per machine inside a sliding
// time window. When the window count crosses the threshold, observe
// returns a KindCrashLoop event. After firing once, the same machine
// is suppressed for the cooldown so a sustained loop doesn't spam.
//
// In-memory only by design: a notifier restart resets the window,
// which is the safe direction (false negative for at most `window`
// minutes, never a false positive carried across restarts).
type crashTracker struct {
	mu        sync.Mutex
	threshold int
	window    time.Duration
	cooldown  time.Duration
	history   map[string][]time.Time
	lastFired map[string]time.Time
}

func newCrashTracker(threshold int, window, cooldown time.Duration) *crashTracker {
	return &crashTracker{
		threshold: threshold,
		window:    window,
		cooldown:  cooldown,
		history:   map[string][]time.Time{},
		lastFired: map[string]time.Time{},
	}
}

// inCooldown reports whether a crash-loop alert has fired for this
// machine recently enough that the cooldown is still active. Callers
// use it to suppress per-crash alerts while a loop is already known —
// the loop alert is the consolidated signal and individual crashes
// during cooldown are noise.
func (c *crashTracker) inCooldown(app, machineID string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.lastFired[app+"/"+machineID]
	if !ok {
		return false
	}
	return now.Sub(last) < c.cooldown
}

func (c *crashTracker) observe(app, machineID, region string, now time.Time) (event.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := app + "/" + machineID
	cutoff := now.Add(-c.window)

	prev := c.history[key]
	keep := prev[:0]
	for _, t := range prev {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	c.history[key] = keep

	if len(keep) < c.threshold {
		return event.Event{}, false
	}
	if last, ok := c.lastFired[key]; ok && now.Sub(last) < c.cooldown {
		return event.Event{}, false
	}
	c.lastFired[key] = now

	return event.Event{
		Kind:      event.KindCrashLoop,
		Severity:  event.SeverityCritical,
		App:       app,
		Region:    region,
		MachineID: machineID,
		Timestamp: now,
		Title:     fmt.Sprintf("%s crash-looping (%s)", machineID, app),
		Detail: fmt.Sprintf(
			"%d crash/OOM events in the last %s on this machine. Likely under-provisioned for current workload — investigate before scaling out.",
			len(keep), c.window,
		),
		Fields: map[string]string{
			"app":     app,
			"machine": machineID,
			"region":  region,
			"count":   fmt.Sprintf("%d", len(keep)),
			"window":  c.window.String(),
		},
	}, true
}
