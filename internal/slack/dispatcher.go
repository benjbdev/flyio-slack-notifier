package slack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
)

type Dispatcher struct {
	WebhookURL  string
	HTTPClient  *http.Client
	DedupWindow time.Duration
	MaxRetries  int
	Logger      *slog.Logger
	Now         func() time.Time

	mu       sync.Mutex
	dedup    map[string]time.Time
}

func NewDispatcher(webhookURL string, dedupWindow time.Duration, logger *slog.Logger) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		WebhookURL:  webhookURL,
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
		DedupWindow: dedupWindow,
		MaxRetries:  4,
		Logger:      logger,
		Now:         time.Now,
		dedup:       map[string]time.Time{},
	}
}

func (d *Dispatcher) Run(ctx context.Context, in <-chan event.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-in:
			if !ok {
				return
			}
			d.handle(ctx, ev)
		}
	}
}

func (d *Dispatcher) handle(ctx context.Context, ev event.Event) {
	if d.isDuplicate(ev) {
		d.Logger.Debug("dedup drop", "kind", ev.Kind, "app", ev.App, "machine", ev.MachineID)
		return
	}
	msg := FormatEvent(ev)
	if err := d.post(ctx, msg); err != nil {
		d.Logger.Error("slack post failed", "err", err, "kind", ev.Kind, "app", ev.App)
	}
}

func (d *Dispatcher) isDuplicate(ev event.Event) bool {
	if d.DedupWindow <= 0 {
		return false
	}
	// Heartbeat-style events must always go through; otherwise a
	// recurring digest with constant fields would collapse to a single
	// message after the first run.
	if ev.Kind == event.KindDigest {
		return false
	}
	key := dedupKey(ev)
	now := d.Now()

	d.mu.Lock()
	defer d.mu.Unlock()

	// purge expired entries opportunistically
	for k, t := range d.dedup {
		if now.Sub(t) > d.DedupWindow {
			delete(d.dedup, k)
		}
	}
	if t, ok := d.dedup[key]; ok && now.Sub(t) <= d.DedupWindow {
		return true
	}
	d.dedup[key] = now
	return false
}

func dedupKey(ev event.Event) string {
	keys := make([]string, 0, len(ev.Fields))
	for k := range ev.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|", ev.Kind, ev.App, ev.MachineID, ev.Region)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s|", k, ev.Fields[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (d *Dispatcher) post(ctx context.Context, msg Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal slack payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= d.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := backoffDelay(attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.WebhookURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := d.HTTPClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()

		retryable := resp.StatusCode == 429 || resp.StatusCode >= 500
		if !retryable {
			return fmt.Errorf("slack post: status %d: %s", resp.StatusCode, string(respBody))
		}
		lastErr = fmt.Errorf("slack post: status %d: %s", resp.StatusCode, string(respBody))

		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(secs) * time.Second):
				}
			}
		}
	}
	return fmt.Errorf("slack post: gave up after %d attempts: %w", d.MaxRetries+1, lastErr)
}

func backoffDelay(attempt int) time.Duration {
	d := time.Duration(1<<attempt) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}
