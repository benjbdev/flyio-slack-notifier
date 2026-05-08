package poller

import (
	"testing"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
)

func TestCrashTrackerEmitsLoopAtThreshold(t *testing.T) {
	c := newCrashTracker(3, 10*time.Minute, 10*time.Minute)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if _, ok := c.observe("api-prod", "m1", "cdg", now); ok {
		t.Fatal("first crash should not emit")
	}
	if _, ok := c.observe("api-prod", "m1", "cdg", now.Add(time.Minute)); ok {
		t.Fatal("second crash should not emit")
	}
	ev, ok := c.observe("api-prod", "m1", "cdg", now.Add(2*time.Minute))
	if !ok {
		t.Fatal("third crash should emit loop")
	}
	if ev.Kind != event.KindCrashLoop {
		t.Errorf("kind = %s", ev.Kind)
	}
	if ev.MachineID != "m1" || ev.App != "api-prod" {
		t.Errorf("event missing identity: %+v", ev)
	}
	if ev.Fields["count"] != "3" {
		t.Errorf("count field = %q, want 3", ev.Fields["count"])
	}
}

func TestCrashTrackerCooldownSuppressesRepeats(t *testing.T) {
	c := newCrashTracker(2, 10*time.Minute, 10*time.Minute)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	c.observe("api-prod", "m1", "cdg", now)
	if _, ok := c.observe("api-prod", "m1", "cdg", now.Add(time.Minute)); !ok {
		t.Fatal("expected loop emit on second crash")
	}
	if _, ok := c.observe("api-prod", "m1", "cdg", now.Add(2*time.Minute)); ok {
		t.Errorf("third crash within cooldown should be suppressed")
	}
	c.observe("api-prod", "m1", "cdg", now.Add(15*time.Minute))
	if _, ok := c.observe("api-prod", "m1", "cdg", now.Add(16*time.Minute)); !ok {
		t.Errorf("after cooldown, fresh threshold should re-fire")
	}
}

func TestCrashTrackerWindowExpiresOldEvents(t *testing.T) {
	c := newCrashTracker(3, 5*time.Minute, time.Hour)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	c.observe("api-prod", "m1", "cdg", now)
	c.observe("api-prod", "m1", "cdg", now.Add(time.Minute))
	if _, ok := c.observe("api-prod", "m1", "cdg", now.Add(10*time.Minute)); ok {
		t.Fatal("first two events fell out of window — should not emit")
	}
}

func TestCrashTrackerInCooldownReflectsLastFired(t *testing.T) {
	c := newCrashTracker(2, 10*time.Minute, 10*time.Minute)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if c.inCooldown("api-prod", "m1", now) {
		t.Error("fresh tracker should not report cooldown")
	}
	c.observe("api-prod", "m1", "cdg", now)
	if c.inCooldown("api-prod", "m1", now) {
		t.Error("cooldown should not engage before threshold crossed")
	}
	c.observe("api-prod", "m1", "cdg", now.Add(time.Minute)) // fires loop
	if !c.inCooldown("api-prod", "m1", now.Add(2*time.Minute)) {
		t.Error("cooldown must be active immediately after loop fires")
	}
	if c.inCooldown("api-prod", "m1", now.Add(15*time.Minute)) {
		t.Error("cooldown must lapse past the configured window")
	}
	if c.inCooldown("api-prod", "m2", now.Add(2*time.Minute)) {
		t.Error("cooldown must be per-machine")
	}
}

func TestCrashTrackerSeparatesPerMachine(t *testing.T) {
	c := newCrashTracker(3, 10*time.Minute, 10*time.Minute)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	c.observe("api-prod", "m1", "cdg", now)
	c.observe("api-prod", "m1", "cdg", now)
	c.observe("api-prod", "m2", "cdg", now)
	if _, ok := c.observe("api-prod", "m2", "cdg", now); ok {
		t.Errorf("m2 should not have crossed threshold")
	}
	if _, ok := c.observe("api-prod", "m1", "cdg", now); !ok {
		t.Errorf("m1 should have crossed threshold")
	}
}
