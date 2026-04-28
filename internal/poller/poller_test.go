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

func TestPollerEmitsNewEventsAfterBootstrap(t *testing.T) {
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
	ch := make(chan event.Event, 8)
	c := flyapi.New(srv.URL, "tk")
	p := New(c, []string{"api-prod"}, time.Second, store, ch, logger)

	if err := p.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.bootstrap = false
	drain(ch)

	current = `[{
		"id":"m1","state":"started","region":"cdg",
		"image_ref":{"repository":"api-prod","tag":"v1"},
		"events":[
			{"id":"e1","type":"start","status":"started","timestamp":1700000000000},
			{"id":"e2","type":"exit","status":"exited","timestamp":1700000050000},
			{"id":"e3","type":"start","status":"started","timestamp":1700000060000}
		]
	}]`

	if err := p.pollAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := collect(ch, 200*time.Millisecond)
	kinds := map[event.Kind]int{}
	for _, ev := range got {
		kinds[ev.Kind]++
	}
	if kinds[event.KindMachineExit] != 1 {
		t.Errorf("expected 1 exit, got %d (all: %+v)", kinds[event.KindMachineExit], kinds)
	}
	if kinds[event.KindMachineStarted] != 1 {
		t.Errorf("expected 1 started, got %d", kinds[event.KindMachineStarted])
	}

	cursor, _ := p.Store.LastSeen("api-prod", "m1")
	if cursor != 1700000060000 {
		t.Errorf("cursor advanced to %d", cursor)
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
