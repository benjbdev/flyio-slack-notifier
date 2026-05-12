package poller

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
)

const (
	// defaultCapacityRealert governs how often we re-emit a "still
	// degraded" alert while running < HWM. Without re-alerts a
	// sustained degradation produces a single message at onset that
	// scrolls past in chat; re-firing keeps the unresolved state
	// visible without operator intervention.
	defaultCapacityRealert = 10 * time.Minute

	// defaultHealthyStreakRequired is how many consecutive observations
	// at running >= HWM are required before declaring "restored". A
	// crash-looping machine bounces between started/stopped each poll;
	// requiring two healthy polls in a row prevents a degraded ↔
	// restored alert pair from firing on every poll.
	defaultHealthyStreakRequired = 2

	// defaultDeploySafetyTimeout caps how long a rolling deploy can
	// suppress capacity alerts. Past this, observe() falls through to
	// normal behavior so a wedged deploy stuck at half-capacity still
	// surfaces as degraded — silent failure is worse than a noisy one.
	defaultDeploySafetyTimeout = 15 * time.Minute
)

// capacityTracker watches per-app counts of running machines and emits
// when the running count drops below the high-water-mark observed
// since startup. Surfaces a min_machines_running shortfall (one of two
// machines vanished and never came back) as an immediate alert instead
// of waiting for the next digest, with periodic re-alerts so a
// long-lived degradation can't get lost in the channel.
//
// In-memory only: a restart re-seeds HWM on the bootstrap pass so we
// never alert against a fictional pre-startup expectation.
type capacityTracker struct {
	mu              sync.Mutex
	hwm             map[string]int
	degraded        map[string]bool
	lastAlertedAt   map[string]time.Time
	healthyStreak   map[string]int
	deployStartedAt map[string]time.Time
	realertInterval time.Duration
	healthyRequired int
	deployTimeout   time.Duration
}

func newCapacityTracker() *capacityTracker {
	return &capacityTracker{
		hwm:             map[string]int{},
		degraded:        map[string]bool{},
		lastAlertedAt:   map[string]time.Time{},
		healthyStreak:   map[string]int{},
		deployStartedAt: map[string]time.Time{},
		realertInterval: defaultCapacityRealert,
		healthyRequired: defaultHealthyStreakRequired,
		deployTimeout:   defaultDeploySafetyTimeout,
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

// observe records the current running count and returns either:
//   - a fresh degradation event (first time below HWM),
//   - a "still degraded" re-alert (running stayed below HWM for
//     longer than realertInterval),
//   - a recovery event (running has been at HWM for healthyRequired
//     consecutive observations), or
//   - (zero, false) when nothing newly transitioned.
//
// The `deploying` flag, when true, suppresses degraded/restored emits
// because a rolling deploy briefly drops running below HWM and isn't an
// outage. The healthyStreak is also reset so a transient spike to HWM
// mid-deploy can't insta-fire "restored" once the deploy clears. To
// guard against a wedged deploy hiding a real outage forever, the
// suppression lifts after deployTimeout and normal alerting resumes.
func (c *capacityTracker) observe(app string, running int, deploying bool, now time.Time) (event.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if running > c.hwm[app] {
		c.hwm[app] = running
	}
	hwm := c.hwm[app]

	if hwm == 0 {
		return event.Event{}, false
	}

	if deploying {
		if c.deployStartedAt[app].IsZero() {
			c.deployStartedAt[app] = now
		}
		if now.Sub(c.deployStartedAt[app]) < c.deployTimeout {
			c.healthyStreak[app] = 0
			return event.Event{}, false
		}
		// Past the safety timeout: fall through so a stuck deploy
		// stranded at half-capacity still surfaces as degraded.
	} else {
		delete(c.deployStartedAt, app)
	}

	if running < hwm {
		c.healthyStreak[app] = 0

		if !c.degraded[app] {
			c.degraded[app] = true
			c.lastAlertedAt[app] = now
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

		// Already degraded — re-alert at most once per realertInterval
		// so an unresolved degradation stays visible in the channel
		// instead of scrolling past after the initial message.
		if last, ok := c.lastAlertedAt[app]; ok && now.Sub(last) >= c.realertInterval {
			elapsed := now.Sub(last).Round(time.Minute)
			c.lastAlertedAt[app] = now
			return event.Event{
				Kind:      event.KindCapacityDegraded,
				Severity:  event.SeverityCritical,
				App:       app,
				Timestamp: now,
				Title:     fmt.Sprintf("%s capacity STILL degraded — %d / %d running", app, running, hwm),
				Detail: fmt.Sprintf(
					"Capacity has been degraded for at least %s with no recovery. %d of %d expected machines running.",
					elapsed, running, hwm,
				),
				Fields: map[string]string{
					"app":      app,
					"running":  strconv.Itoa(running),
					"expected": strconv.Itoa(hwm),
					"elapsed":  elapsed.String(),
				},
			}, true
		}
		return event.Event{}, false
	}

	// running >= hwm: healthy this poll. Only declare "restored"
	// after healthyRequired consecutive observations to ride out
	// crash-loop flap.
	if c.degraded[app] {
		c.healthyStreak[app]++
		if c.healthyStreak[app] < c.healthyRequired {
			return event.Event{}, false
		}
		c.degraded[app] = false
		c.healthyStreak[app] = 0
		delete(c.lastAlertedAt, app)
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
