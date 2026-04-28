package slack

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/becreacom/flyio-slack/internal/event"
)

func newTestDispatcher(t *testing.T, url string) *Dispatcher {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := NewDispatcher(url, time.Minute, logger)
	d.MaxRetries = 2
	d.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	return d
}

func TestDispatcherPostsAndFormats(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	d.handle(context.Background(), event.Event{
		Kind:      event.KindMachineOOM,
		Severity:  event.SeverityCritical,
		App:       "api-prod",
		MachineID: "m1",
		Region:    "cdg",
		Title:     "OOM-killed",
		Detail:    "out of memory",
		Fields:    map[string]string{"app": "api-prod", "machine": "m1"},
	})

	if len(got) == 0 {
		t.Fatal("no body received")
	}
	var msg Message
	if err := json.Unmarshal(got, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Text != "OOM-killed" {
		t.Errorf("fallback text = %q", msg.Text)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments len = %d", len(msg.Attachments))
	}
	if msg.Attachments[0].Color != colorCritical {
		t.Errorf("color = %q", msg.Attachments[0].Color)
	}
}

func TestDispatcherDedup(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	ev := event.Event{
		Kind: event.KindMachineStarted, App: "a", MachineID: "m1",
		Title: "started",
		Fields: map[string]string{"machine": "m1"},
	}
	d.handle(context.Background(), ev)
	d.handle(context.Background(), ev)
	d.handle(context.Background(), ev)

	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 POST after dedup, got %d", got)
	}
}

func TestDispatcherDedupExpiresAfterWindow(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	d.DedupWindow = 100 * time.Millisecond

	frozen := time.Now()
	d.Now = func() time.Time { return frozen }

	ev := event.Event{Kind: event.KindMachineStarted, App: "a", MachineID: "m1", Title: "x"}
	d.handle(context.Background(), ev)

	frozen = frozen.Add(200 * time.Millisecond) // past dedup window
	d.handle(context.Background(), ev)

	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 POSTs after dedup expiry, got %d", got)
	}
}

func TestDispatcherRetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	d.handle(context.Background(), event.Event{
		Kind: event.KindDeploy, App: "x", Title: "t",
	})

	if got := calls.Load(); got < 2 {
		t.Errorf("expected at least 2 attempts (retry), got %d", got)
	}
}

func TestDispatcherDoesNotRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad"))
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	d.handle(context.Background(), event.Event{
		Kind: event.KindDeploy, App: "x", Title: "t",
	})

	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 attempt for non-retryable status, got %d", got)
	}
}

func TestDispatcherDoesNotDedupDigest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	d.DedupWindow = time.Hour // even with a long window, digests must always go through

	ev := event.Event{
		Kind:    event.KindDigest,
		Title:   "Fly.io status digest — 12:00 UTC",
		Payload: nil,
	}
	for i := 0; i < 5; i++ {
		d.handle(context.Background(), ev)
	}

	if got := calls.Load(); got != 5 {
		t.Errorf("expected 5 digest POSTs (no dedup), got %d", got)
	}
}

func TestDispatcherRunsAndStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, srv.URL)
	in := make(chan event.Event, 4)
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.Run(ctx, in)
	}()

	in <- event.Event{Kind: event.KindDeploy, App: "x", Title: "t"}
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()
}

func TestFormatEventAlertHasValidContextBlock(t *testing.T) {
	// Slack rejects context blocks that use `text` instead of `elements`,
	// returning 400 invalid_attachments. Guard against regression.
	msg := FormatEvent(event.Event{
		Kind:      event.KindMachineStarted,
		Severity:  event.SeverityInfo,
		App:       "api-prod",
		MachineID: "m1",
		Title:     "started",
	})
	if len(msg.Attachments) == 0 {
		t.Fatal("expected attachments")
	}

	var foundContext bool
	for _, b := range msg.Attachments[0].Blocks {
		if b.Type != "context" {
			continue
		}
		foundContext = true
		if b.Text != nil {
			t.Errorf("context block must not have text field; use elements")
		}
		if len(b.Elements) == 0 {
			t.Errorf("context block must have non-empty elements")
		}
	}
	if !foundContext {
		t.Error("expected a context block (dashboard link)")
	}
}

func TestFormatEventDigest(t *testing.T) {
	// Smoke test that digest events produce valid JSON.
	ev := event.Event{
		Kind:      event.KindDigest,
		Severity:  event.SeverityInfo,
		Title:     "Fly.io status digest — 12:00 UTC",
		Detail:    "no apps configured",
		Timestamp: time.Now(),
	}
	msg := FormatEvent(ev)
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Fly.io status digest") {
		t.Errorf("digest title missing from payload: %s", string(b))
	}
}
