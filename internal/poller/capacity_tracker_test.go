package poller

import (
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

	rec, ok := c.observe("api-prod", 2, now)
	if !ok || rec.Kind != event.KindCapacityRestored {
		t.Fatalf("expected restored event, got ok=%v kind=%s", ok, rec.Kind)
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
