package poller

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
	"github.com/benjbdev/flyio-slack-notifier/internal/flyapi"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestPoller(t *testing.T, payload string, ch chan event.Event) (*Poller, func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, payload)
	}))
	store := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)
	return p, srv.Close
}

func TestPollerBootstrapDoesNotEmit(t *testing.T) {
	payload := `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[
			{"id":"e1","type":"start","status":"started","timestamp":1700000000000}
		]
	}]`

	ch := make(chan event.Event, 8)
	p, close := newTestPoller(t, payload, ch)
	defer close()

	if err := p.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-ch:
		t.Errorf("bootstrap should not emit, got %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	cursor, _ := p.Store.LastSeen("api-prod", "m1")
	if cursor != 1700000000000 {
		t.Errorf("cursor = %d", cursor)
	}
}

// Routine lifecycle events (start, restart, launch, update, plain
// exit without payload) are signal-free and must be silently dropped.
// Cursor still advances so we don't replay them on the next poll.
func TestPollerSilentOnRoutineLifecycleEvents(t *testing.T) {
	first := `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[
			{"id":"e1","type":"start","status":"started","timestamp":1700000000000}
		]
	}]`
	var current = first
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, current)
	}))
	defer srv.Close()

	store := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := make(chan event.Event, 16)
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)

	if err := p.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.bootstrap = false
	drain(ch)

	// Mix of every routine-noise type that previously hit Slack.
	current = `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[
			{"id":"e1","type":"start","status":"started","timestamp":1700000000000},
			{"id":"e2","type":"exit","status":"exited","timestamp":1700000050000},
			{"id":"e3","type":"start","status":"started","timestamp":1700000060000},
			{"id":"e4","type":"start","status":"starting","timestamp":1700000070000},
			{"id":"e5","type":"restart","status":"starting","timestamp":1700000080000},
			{"id":"e6","type":"launch","status":"created","timestamp":1700000090000},
			{"id":"e7","type":"launch","status":"pending","timestamp":1700000100000},
			{"id":"e8","type":"update","status":"replacing","timestamp":1700000110000},
			{"id":"e9","type":"update","status":"replaced","timestamp":1700000120000},
			{"id":"e10","type":"update","status":"stopped","timestamp":1700000130000}
		]
	}]`

	if err := p.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := collect(ch, 100*time.Millisecond); len(got) != 0 {
		t.Errorf("routine lifecycle events must be silent, got %d events: %+v", len(got), got)
	}

	cursor, _ := p.Store.LastSeen("api-prod", "m1")
	if cursor != 1700000130000 {
		t.Errorf("cursor should advance past all routine events, got %d", cursor)
	}
}

func TestPollerDeployDetection(t *testing.T) {
	v1 := `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[]
	}]`
	v2 := `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v2"},
		"events":[]
	}]`

	current := v1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, current)
	}))
	defer srv.Close()

	store := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := make(chan event.Event, 8)
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)

	// bootstrap
	_ = p.pollAll(context.Background())
	p.bootstrap = false
	drain(ch)

	// no change => no deploy event
	_ = p.pollAll(context.Background())
	if got := collect(ch, 50*time.Millisecond); len(got) != 0 {
		t.Errorf("unexpected events on no-change poll: %+v", got)
	}

	// image change => one deploy event
	current = v2
	_ = p.pollAll(context.Background())
	got := collect(ch, 100*time.Millisecond)
	if len(got) != 1 || got[0].Kind != event.KindDeploy {
		t.Errorf("expected one deploy event, got %+v", got)
	}
}

func TestPollerEmitsOOMFromExitPayload(t *testing.T) {
	first := `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[{"id":"e1","type":"start","status":"started","timestamp":1700000000000}]
	}]`
	second := `[{
		"id":"m1","state":"stopped","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[
			{"id":"e1","type":"start","status":"started","timestamp":1700000000000},
			{"id":"e2","type":"exit","status":"stopped","timestamp":1700000050000,
			 "request":{"exit_event":{"exit_code":137,"guest_signal":-1,"oom_killed":true,"requested_stop":false}}}
		]
	}]`

	current := first
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, current)
	}))
	defer srv.Close()

	store := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := make(chan event.Event, 8)
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)

	_ = p.pollAll(context.Background())
	p.bootstrap = false
	drain(ch)

	current = second
	_ = p.pollAll(context.Background())
	got := collect(ch, 100*time.Millisecond)

	var sawOOM bool
	for _, ev := range got {
		if ev.Kind == event.KindMachineOOM {
			sawOOM = true
			if ev.Severity != event.SeverityCritical {
				t.Errorf("OOM severity = %s, want critical", ev.Severity)
			}
			if ev.Fields["oom_killed"] != "true" || ev.Fields["exit_code"] != "137" {
				t.Errorf("OOM fields incomplete: %+v", ev.Fields)
			}
		}
	}
	if !sawOOM {
		t.Errorf("expected OOM event, got %+v", got)
	}
}

func TestPollerEmitsCrashedOnNonZeroExit(t *testing.T) {
	first := `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[{"id":"e1","type":"start","status":"started","timestamp":1700000000000}]
	}]`
	second := `[{
		"id":"m1","state":"stopped","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[
			{"id":"e1","type":"start","status":"started","timestamp":1700000000000},
			{"id":"e2","type":"exit","status":"stopped","timestamp":1700000050000,
			 "request":{"exit_event":{"exit_code":139,"guest_signal":11,"oom_killed":false,"requested_stop":false}}}
		]
	}]`

	current := first
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, current)
	}))
	defer srv.Close()

	store := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := make(chan event.Event, 8)
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)

	_ = p.pollAll(context.Background())
	p.bootstrap = false
	drain(ch)

	current = second
	_ = p.pollAll(context.Background())
	got := collect(ch, 100*time.Millisecond)

	var sawCrashed bool
	for _, ev := range got {
		if ev.Kind == event.KindMachineCrashed {
			sawCrashed = true
			if ev.Severity != event.SeverityCritical {
				t.Errorf("crashed severity = %s", ev.Severity)
			}
			if ev.Fields["exit_code"] != "139" {
				t.Errorf("exit_code = %q", ev.Fields["exit_code"])
			}
		}
	}
	if !sawCrashed {
		t.Errorf("expected crashed event, got %+v", got)
	}
}

func TestPollerCleanExitWhenRequestedStop(t *testing.T) {
	first := `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[{"id":"e1","type":"start","status":"started","timestamp":1700000000000}]
	}]`
	second := `[{
		"id":"m1","state":"stopped","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[
			{"id":"e1","type":"start","status":"started","timestamp":1700000000000},
			{"id":"e2","type":"exit","status":"stopped","timestamp":1700000050000,
			 "request":{"exit_event":{"exit_code":0,"requested_stop":true}}}
		]
	}]`

	current := first
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, current)
	}))
	defer srv.Close()

	store := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := make(chan event.Event, 8)
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)

	_ = p.pollAll(context.Background())
	p.bootstrap = false
	drain(ch)

	current = second
	_ = p.pollAll(context.Background())
	got := collect(ch, 100*time.Millisecond)

	for _, ev := range got {
		if ev.Kind == event.KindMachineOOM || ev.Kind == event.KindMachineCrashed {
			t.Errorf("requested stop classified as %s: %+v", ev.Kind, ev)
		}
	}
}

func TestPollerEmitsCapacityDegraded(t *testing.T) {
	twoUp := `[
		{"id":"m1","state":"started","region":"cdg","image_ref":{"repository":"api-prod","tag":"v1"},"events":[]},
		{"id":"m2","state":"started","region":"cdg","image_ref":{"repository":"api-prod","tag":"v1"},"events":[]}
	]`
	oneUp := `[
		{"id":"m1","state":"started","region":"cdg","image_ref":{"repository":"api-prod","tag":"v1"},"events":[]},
		{"id":"m2","state":"stopped","region":"cdg","image_ref":{"repository":"api-prod","tag":"v1"},"events":[]}
	]`

	current := twoUp
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, current)
	}))
	defer srv.Close()

	store := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := make(chan event.Event, 8)
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)

	_ = p.pollAll(context.Background())
	p.bootstrap = false
	drain(ch)

	current = oneUp
	_ = p.pollAll(context.Background())
	got := collect(ch, 100*time.Millisecond)

	var sawDegraded bool
	for _, ev := range got {
		if ev.Kind == event.KindCapacityDegraded {
			sawDegraded = true
		}
	}
	if !sawDegraded {
		t.Errorf("expected capacity degraded event, got %+v", got)
	}
}

// Once the crash tracker has fired a crash-loop alert, individual
// per-crash events for that machine should be suppressed until the
// cooldown elapses — the loop alert is the consolidated signal.
func TestPollerSuppressesIndividualCrashesDuringLoop(t *testing.T) {
	first := `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[{"id":"e0","type":"start","status":"started","timestamp":1700000000000}]
	}]`

	current := first
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, current)
	}))
	defer srv.Close()

	store := newTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ch := make(chan event.Event, 32)
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)

	_ = p.pollAll(context.Background())
	p.bootstrap = false
	drain(ch)

	// Six crashes in quick succession, all with non-zero exit. With
	// the default threshold of 3, the loop fires on crash #3; crashes
	// #4-#6 should be suppressed (only crashes #1, #2, #3 + 1 loop emit).
	current = `[{
		"id":"m1","state":"stopped","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[
			{"id":"e0","type":"start","status":"started","timestamp":1700000000000},
			{"id":"e1","type":"exit","status":"stopped","timestamp":1700000010000,
			 "request":{"exit_event":{"exit_code":1,"requested_stop":false}}},
			{"id":"e2","type":"exit","status":"stopped","timestamp":1700000020000,
			 "request":{"exit_event":{"exit_code":1,"requested_stop":false}}},
			{"id":"e3","type":"exit","status":"stopped","timestamp":1700000030000,
			 "request":{"exit_event":{"exit_code":1,"requested_stop":false}}},
			{"id":"e4","type":"exit","status":"stopped","timestamp":1700000040000,
			 "request":{"exit_event":{"exit_code":1,"requested_stop":false}}},
			{"id":"e5","type":"exit","status":"stopped","timestamp":1700000050000,
			 "request":{"exit_event":{"exit_code":1,"requested_stop":false}}},
			{"id":"e6","type":"exit","status":"stopped","timestamp":1700000060000,
			 "request":{"exit_event":{"exit_code":1,"requested_stop":false}}}
		]
	}]`

	_ = p.pollAll(context.Background())
	got := collect(ch, 200*time.Millisecond)

	kinds := map[event.Kind]int{}
	for _, ev := range got {
		kinds[ev.Kind]++
	}
	if kinds[event.KindMachineCrashed] != 3 {
		t.Errorf("expected 3 individual crash events (pre-loop), got %d (all: %+v)", kinds[event.KindMachineCrashed], kinds)
	}
	if kinds[event.KindCrashLoop] != 1 {
		t.Errorf("expected 1 crash-loop event, got %d", kinds[event.KindCrashLoop])
	}
}

func drain(ch chan event.Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func collect(ch chan event.Event, d time.Duration) []event.Event {
	deadline := time.After(d)
	var out []event.Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
}
