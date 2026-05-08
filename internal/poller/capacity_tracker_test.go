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
	if _, ok := c.observe("api-prod", 2, now); ok {
		t.Errorf("steady state should not emit")
	}
}

func TestCapacityTrackerEmitsDegradedAndRestored(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	now := time.Now()

	ev, ok := c.observe("api-prod", 1, now)
	if !ok || ev.Kind != event.KindCapacityDegraded {
		t.Fatalf("expected degraded event, got ok=%v kind=%s", ok, ev.Kind)
	}
	if ev.Fields["running"] != "1" || ev.Fields["expected"] != "2" {
		t.Errorf("fields wrong: %+v", ev.Fields)
	}

	if _, ok := c.observe("api-prod", 1, now); ok {
		t.Errorf("repeated observation while degraded should not re-emit")
	}

	// First healthy observation builds the recovery streak but does
	// not fire — flap protection.
	if _, ok := c.observe("api-prod", 2, now); ok {
		t.Errorf("first healthy observation should not yet declare restored")
	}
	rec, ok := c.observe("api-prod", 2, now)
	if !ok || rec.Kind != event.KindCapacityRestored {
		t.Fatalf("expected restored event after second healthy poll, got ok=%v kind=%s", ok, rec.Kind)
	}
}

func TestCapacityTrackerSuppressesFlap(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	now := time.Now()

	// degraded → emits once
	if _, ok := c.observe("api-prod", 1, now); !ok {
		t.Fatal("expected initial degraded emit")
	}
	// quick bounce back to 2/2: should NOT emit restored yet
	if ev, ok := c.observe("api-prod", 2, now.Add(30*time.Second)); ok {
		t.Errorf("single healthy poll declared restored prematurely: %+v", ev)
	}
	// crash-loop bounces it back down: degraded flag still set, no
	// duplicate alert (same app, still inside re-alert window)
	if _, ok := c.observe("api-prod", 1, now.Add(60*time.Second)); ok {
		t.Errorf("re-degradation within window must not re-emit immediately")
	}
}

func TestCapacityTrackerRealertsWhenStillDegraded(t *testing.T) {
	c := newCapacityTracker()
	c.realertInterval = 5 * time.Minute
	c.seed("api-prod", 2)
	now := time.Now()

	if _, ok := c.observe("api-prod", 1, now); !ok {
		t.Fatal("expected initial degraded emit")
	}

	// Inside re-alert window: silent.
	if _, ok := c.observe("api-prod", 1, now.Add(2*time.Minute)); ok {
		t.Errorf("should not re-alert before realertInterval elapsed")
	}

	// Past re-alert window: still-degraded fires.
	ev, ok := c.observe("api-prod", 1, now.Add(6*time.Minute))
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

	if _, ok := c.observe("api-prod", 3, now); ok {
		t.Errorf("growth above HWM should not emit")
	}
	if _, ok := c.observe("api-prod", 2, now); !ok {
		t.Errorf("running=2 with HWM=3 should emit degraded")
	}
}

func TestCapacityTrackerSeparatesPerApp(t *testing.T) {
	c := newCapacityTracker()
	c.seed("api-prod", 2)
	c.seed("worker", 1)
	now := time.Now()

	if _, ok := c.observe("worker", 1, now); ok {
		t.Errorf("worker steady should not emit")
	}
	ev, ok := c.observe("api-prod", 1, now)
	if !ok || ev.App != "api-prod" {
		t.Fatalf("expected api-prod degraded, got ok=%v app=%s", ok, ev.App)
	}
}

func TestCapacityTrackerNoEmitWhenNothingSeeded(t *testing.T) {
	c := newCapacityTracker()
	if _, ok := c.observe("brand-new", 0, time.Now()); ok {
		t.Errorf("zero hwm with zero running should not emit")
	}
}
