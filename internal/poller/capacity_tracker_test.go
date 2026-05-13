package poller

import (
	"strings"
	"testing"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
)

func TestCapacityTrackerSeedDoesNotEmit(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)

	now := time.Now()
	if _, ok := c.observe("api-prod", 2, false, now); ok {
		t.Errorf("steady state should not emit")
	}
}

func TestCapacityTrackerEmitsDegradedAndRestored(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	now := time.Now()

	// First running<HWM observation builds the degraded streak but
	// does not yet fire — hysteresis against Fly's rolling-deploy
	// window where the running count drops before the new image_ref
	// is visible.
	if _, ok := c.observe("api-prod", 1, false, now); ok {
		t.Errorf("first sub-HWM observation should not yet fire degraded")
	}
	ev, ok := c.observe("api-prod", 1, false, now)
	if !ok || ev.Kind != event.KindCapacityDegraded {
		t.Fatalf("expected degraded event on second sub-HWM observation, got ok=%v kind=%s", ok, ev.Kind)
	}
	if ev.Fields["running"] != "1" || ev.Fields["expected"] != "2" {
		t.Errorf("fields wrong: %+v", ev.Fields)
	}

	if _, ok := c.observe("api-prod", 1, false, now); ok {
		t.Errorf("repeated observation while degraded should not re-emit")
	}

	// First healthy observation builds the recovery streak but does
	// not fire — flap protection.
	if _, ok := c.observe("api-prod", 2, false, now); ok {
		t.Errorf("first healthy observation should not yet declare restored")
	}
	rec, ok := c.observe("api-prod", 2, false, now)
	if !ok || rec.Kind != event.KindCapacityRestored {
		t.Fatalf("expected restored event after second healthy poll, got ok=%v kind=%s", ok, rec.Kind)
	}
}

func TestCapacityTrackerSuppressesFlap(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	now := time.Now()

	// degraded → emits on second sub-HWM observation (hysteresis)
	if _, ok := c.observe("api-prod", 1, false, now); ok {
		t.Fatal("first observation must not fire (hysteresis)")
	}
	if _, ok := c.observe("api-prod", 1, false, now); !ok {
		t.Fatal("expected degraded emit on second observation")
	}
	// quick bounce back to 2/2: should NOT emit restored yet
	if ev, ok := c.observe("api-prod", 2, false, now.Add(30*time.Second)); ok {
		t.Errorf("single healthy poll declared restored prematurely: %+v", ev)
	}
	// crash-loop bounces it back down: degraded flag still set, no
	// duplicate alert (same app, still inside re-alert window)
	if _, ok := c.observe("api-prod", 1, false, now.Add(60*time.Second)); ok {
		t.Errorf("re-degradation within window must not re-emit immediately")
	}
}

func TestCapacityTrackerRealertsWhenStillDegraded(t *testing.T) {
	c := newCapacityTracker()
	c.realertInterval = 5 * time.Minute
	c.seed("api-prod", 2)
	now := time.Now()

	if _, ok := c.observe("api-prod", 1, false, now); ok {
		t.Fatal("first observation must not fire (hysteresis)")
	}
	if _, ok := c.observe("api-prod", 1, false, now); !ok {
		t.Fatal("expected degraded emit on second observation")
	}

	// Inside re-alert window: silent.
	if _, ok := c.observe("api-prod", 1, false, now.Add(2*time.Minute)); ok {
		t.Errorf("should not re-alert before realertInterval elapsed")
	}

	// Past re-alert window: still-degraded fires.
	ev, ok := c.observe("api-prod", 1, false, now.Add(6*time.Minute))
	if !ok || ev.Kind != event.KindCapacityDegraded {
		t.Fatalf("expected still-degraded re-alert, got ok=%v kind=%s", ok, ev.Kind)
	}
	if !strings.Contains(ev.Title, "STILL") {
		t.Errorf("re-alert title should signal persistence, got %q", ev.Title)
	}
	if ev.Fields["elapsed"] == "" {
		t.Errorf("re-alert should include elapsed field, got %+v", ev.Fields)
	}
}

func TestCapacityTrackerRaisesHWMOnGrowth(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	now := time.Now()

	if _, ok := c.observe("api-prod", 3, false, now); ok {
		t.Errorf("growth above HWM should not emit")
	}
	if _, ok := c.observe("api-prod", 2, false, now); ok {
		t.Errorf("first sub-HWM observation should not fire (hysteresis)")
	}
	if _, ok := c.observe("api-prod", 2, false, now); !ok {
		t.Errorf("second observation at running=2 with HWM=3 should emit degraded")
	}
}

func TestCapacityTrackerSeparatesPerApp(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	c.seed("worker", 1)
	now := time.Now()

	if _, ok := c.observe("worker", 1, false, now); ok {
		t.Errorf("worker steady should not emit")
	}
	if _, ok := c.observe("api-prod", 1, false, now); ok {
		t.Fatal("first sub-HWM observation must not fire (hysteresis)")
	}
	ev, ok := c.observe("api-prod", 1, false, now)
	if !ok || ev.App != "api-prod" {
		t.Fatalf("expected api-prod degraded, got ok=%v app=%s", ok, ev.App)
	}
}

func TestCapacityTrackerNoEmitWhenNothingSeeded(t *testing.T) {
	c := newCapacityTracker()
	if _, ok := c.observe("brand-new", 0, false, time.Now()); ok {
		t.Errorf("zero hwm with zero running should not emit")
	}
}

// A rolling deploy briefly drops running below HWM with the deploying
// flag set. We must NOT alert "degraded" — and on the way out, when
// running returns to HWM, we must NOT alert "restored" either (we never
// went degraded).
func TestCapacityTrackerSuppressesDegradedDuringDeploy(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	now := time.Now()

	// Mid-deploy: running drops, deploying=true. Silent.
	if ev, ok := c.observe("api-prod", 1, true, now); ok {
		t.Errorf("rolling deploy must not fire degraded, got %+v", ev)
	}
	// Mid-deploy still rolling: running back to 2 but image mix
	// persists. Silent.
	if ev, ok := c.observe("api-prod", 2, true, now.Add(15*time.Second)); ok {
		t.Errorf("rolling deploy must not fire any capacity event, got %+v", ev)
	}
	// Second machine cycles: running drops again. Still silent.
	if ev, ok := c.observe("api-prod", 1, true, now.Add(30*time.Second)); ok {
		t.Errorf("second leg of rolling deploy must stay silent, got %+v", ev)
	}
	// Deploy completes: deploying=false, running=2. We never went
	// degraded → no restored should fire (and even after two healthy
	// polls).
	if ev, ok := c.observe("api-prod", 2, false, now.Add(45*time.Second)); ok {
		t.Errorf("post-deploy first poll must not fire, got %+v", ev)
	}
	if ev, ok := c.observe("api-prod", 2, false, now.Add(60*time.Second)); ok {
		t.Errorf("post-deploy steady state must not fire restored (never went degraded), got %+v", ev)
	}
}

// Safety net: a deploy stuck at half-capacity for longer than
// deployTimeout must eventually surface as degraded. Silent failure is
// worse than a noisy one.
func TestCapacityTrackerDeploySafetyTimeoutFiresDegraded(t *testing.T) {
	c := newCapacityTracker()
	c.deployTimeout = 5 * time.Minute
	c.seed("api-prod", 2)
	now := time.Now()

	// Suppressed within timeout.
	if _, ok := c.observe("api-prod", 1, true, now); ok {
		t.Errorf("must suppress within deploy timeout")
	}
	if _, ok := c.observe("api-prod", 1, true, now.Add(2*time.Minute)); ok {
		t.Errorf("must stay suppressed mid-window")
	}

	// Past timeout, the suppression lifts but degraded hysteresis
	// still requires two observations to gate the first transition.
	if _, ok := c.observe("api-prod", 1, true, now.Add(6*time.Minute)); ok {
		t.Errorf("first post-timeout observation must not fire (hysteresis)")
	}
	ev, ok := c.observe("api-prod", 1, true, now.Add(6*time.Minute+30*time.Second))
	if !ok || ev.Kind != event.KindCapacityDegraded {
		t.Fatalf("expected degraded after safety timeout + hysteresis, got ok=%v kind=%s", ok, ev.Kind)
	}
}

// If a deploy is detected while we were already degraded from a real
// outage, the still-degraded re-alert is paused during the deploy and
// resumes once the deploy clears.
func TestCapacityTrackerPausesRealertDuringDeploy(t *testing.T) {
	c := newCapacityTracker()
	c.realertInterval = 5 * time.Minute
	c.seed("api-prod", 2)
	now := time.Now()

	// Real outage: degraded fires after hysteresis (2 polls).
	if _, ok := c.observe("api-prod", 1, false, now); ok {
		t.Fatal("first sub-HWM observation must not fire")
	}
	if _, ok := c.observe("api-prod", 1, false, now); !ok {
		t.Fatal("expected degraded emit on second observation")
	}
	// Operator deploys a fix; deploying=true now suppresses the
	// re-alert that would otherwise fire past realertInterval.
	if ev, ok := c.observe("api-prod", 1, true, now.Add(6*time.Minute)); ok {
		t.Errorf("deploy in progress must pause still-degraded re-alert, got %+v", ev)
	}
}

// Reproduces the real-world scenario the divergence-only fix missed:
// Fly drops the running count to 1 BEFORE the new image_ref is
// visible in the API. Without hysteresis the first poll fires
// degraded; with hysteresis the second poll sees the divergence and
// flips deploying=true, suppressing the alert entirely.
func TestCapacityTrackerHysteresisCoversZeroDivergenceWindow(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	now := time.Now()

	// Poll 1: machine dropped, but Fly hasn't surfaced the new image
	// yet — deploying flag is still false. Hysteresis must hold the
	// alert.
	if ev, ok := c.observe("api-prod", 1, false, now); ok {
		t.Fatalf("first sub-HWM poll must not fire (hysteresis); got %+v", ev)
	}
	// Poll 2 (30s later): image divergence now visible, deploying=true.
	// Streak resets, no degraded fires.
	if ev, ok := c.observe("api-prod", 1, true, now.Add(30*time.Second)); ok {
		t.Errorf("deploy detected on second poll must suppress degraded, got %+v", ev)
	}
	// Poll 3: deploy progressing, running=2 with mixed images.
	if ev, ok := c.observe("api-prod", 2, true, now.Add(60*time.Second)); ok {
		t.Errorf("mid-deploy at HWM must stay silent, got %+v", ev)
	}
	// Poll 4: deploy complete, refs converged.
	if ev, ok := c.observe("api-prod", 2, false, now.Add(90*time.Second)); ok {
		t.Errorf("post-deploy first poll must not fire, got %+v", ev)
	}
	// Poll 5: steady state. No restored because we never went degraded.
	if ev, ok := c.observe("api-prod", 2, false, now.Add(120*time.Second)); ok {
		t.Errorf("post-deploy steady state must not fire restored, got %+v", ev)
	}
}

// A transient single-poll dip — running drops then immediately
// recovers — must not accumulate toward a later alert. Verifies the
// degraded streak resets on healthy observations.
func TestCapacityTrackerHysteresisResetsOnRecovery(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	now := time.Now()

	// Single dip then recovery.
	if _, ok := c.observe("api-prod", 1, false, now); ok {
		t.Fatal("first dip must not fire")
	}
	if _, ok := c.observe("api-prod", 2, false, now.Add(30*time.Second)); ok {
		t.Fatal("recovery must not fire restored (never went degraded)")
	}
	// Another single dip — streak should have reset, so this is
	// streak=1 again, not streak=2.
	if ev, ok := c.observe("api-prod", 1, false, now.Add(60*time.Second)); ok {
		t.Fatalf("second isolated dip must not fire (streak should have reset), got %+v", ev)
	}
}
