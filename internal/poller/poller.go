package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
	"github.com/benjbdev/flyio-slack-notifier/internal/flyapi"
)

type Poller struct {
	Client   *flyapi.Client
	Apps     []string
	Interval time.Duration
	Store    *Store
	Out      chan<- event.Event
	Logger   *slog.Logger

	// bootstrap=true on the very first poll: we set cursors to the
	// current latest event per machine without emitting, so we don't
	// spam Slack with the historical event log on startup.
	bootstrap bool
}

func New(client *flyapi.Client, apps []string, interval time.Duration, store *Store, out chan<- event.Event, logger *slog.Logger) *Poller {
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{
		Client:    client,
		Apps:      apps,
		Interval:  interval,
		Store:     store,
		Out:       out,
		Logger:    logger,
		bootstrap: true,
	}
}

func (p *Poller) Run(ctx context.Context) error {
	t := time.NewTicker(p.Interval)
	defer t.Stop()

	if err := p.pollAll(ctx); err != nil {
		p.Logger.Warn("initial poll failed", "err", err)
	}
	p.bootstrap = false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := p.pollAll(ctx); err != nil {
				p.Logger.Warn("poll failed", "err", err)
			}
		}
	}
}

func (p *Poller) pollAll(ctx context.Context) error {
	for _, app := range p.Apps {
		if err := p.pollApp(ctx, app); err != nil {
			p.Logger.Warn("poll app failed", "app", app, "err", err)
		}
	}
	return nil
}

func (p *Poller) pollApp(ctx context.Context, app string) error {
	machines, err := p.Client.ListMachines(ctx, app)
	if err != nil {
		return err
	}

	p.detectDeploy(app, machines)

	for _, m := range machines {
		if err := p.processMachineEvents(app, m); err != nil {
			p.Logger.Warn("process machine events", "app", app, "machine", m.ID, "err", err)
		}
	}
	return nil
}

// detectDeploy emits a single deploy event when the dominant image_ref
// across an app's machines changes vs. what we recorded last poll.
func (p *Poller) detectDeploy(app string, machines []flyapi.Machine) {
	current := dominantImage(machines)
	if current == "" {
		return
	}

	prev, err := p.Store.LastSeen(app, "__image__")
	if err != nil {
		p.Logger.Warn("read image cursor", "app", app, "err", err)
		return
	}
	prevImage, _ := p.Store.GetMeta(app, "image_ref")

	if prev == 0 && prevImage == "" {
		// first time seeing this app — record without emitting.
		_ = p.Store.SetMeta(app, "image_ref", current)
		_ = p.Store.SetLastSeen(app, "__image__", time.Now().UnixMilli())
		return
	}

	if prevImage == current {
		return
	}

	if !p.bootstrap {
		p.emit(event.Event{
			Kind:      event.KindDeploy,
			Severity:  event.SeverityInfo,
			App:       app,
			Timestamp: time.Now(),
			Title:     fmt.Sprintf("%s deployed", app),
			Detail:    fmt.Sprintf("image: %s", current),
			Fields: map[string]string{
				"app":       app,
				"image":     current,
				"prev":      prevImage,
				"machines":  fmt.Sprintf("%d", len(machines)),
			},
		})
	}
	_ = p.Store.SetMeta(app, "image_ref", current)
	_ = p.Store.SetLastSeen(app, "__image__", time.Now().UnixMilli())
}

func dominantImage(machines []flyapi.Machine) string {
	counts := map[string]int{}
	var best string
	bestN := 0
	for _, m := range machines {
		ref := m.ImageRef.String()
		if ref == "" {
			ref = m.Config.Image
		}
		if ref == "" {
			continue
		}
		counts[ref]++
		if counts[ref] > bestN {
			bestN = counts[ref]
			best = ref
		}
	}
	return best
}

func (p *Poller) processMachineEvents(app string, m flyapi.Machine) error {
	cursor, err := p.Store.LastSeen(app, m.ID)
	if err != nil {
		return err
	}

	var newest int64 = cursor
	newCount := 0
	for _, e := range m.Events {
		if e.Timestamp <= cursor {
			continue
		}
		if e.Timestamp > newest {
			newest = e.Timestamp
		}
		newCount++
		if p.bootstrap {
			continue
		}
		p.Logger.Info("machine event",
			"app", app,
			"machine", m.ID,
			"type", e.Type,
			"status", e.Status,
			"source", e.Source,
			"event_id", e.ID,
		)
		if ev, ok := mapMachineEvent(app, m, e); ok {
			p.emit(ev)
		}
	}

	if newCount > 0 && p.bootstrap {
		p.Logger.Debug("bootstrap recorded events", "app", app, "machine", m.ID, "count", newCount)
	}

	if newest > cursor {
		if err := p.Store.SetLastSeen(app, m.ID, newest); err != nil {
			return err
		}
	}
	return nil
}

func mapMachineEvent(app string, m flyapi.Machine, e flyapi.MachineEvent) (event.Event, bool) {
	t := strings.ToLower(e.Type)
	s := strings.ToLower(e.Status)

	base := event.Event{
		App:       app,
		Region:    m.Region,
		MachineID: m.ID,
		Timestamp: e.Time(),
		Fields: map[string]string{
			"app":      app,
			"machine":  m.ID,
			"region":   m.Region,
			"type":     e.Type,
			"status":   e.Status,
			"source":   e.Source,
			"event_id": e.ID,
		},
	}

	switch {
	case strings.Contains(t, "oom") || strings.Contains(s, "oom") || strings.Contains(s, "out-of-memory"):
		base.Kind = event.KindMachineOOM
		base.Severity = event.SeverityCritical
		base.Title = fmt.Sprintf("%s OOM-killed (%s)", m.ID, app)
		base.Detail = "Machine ran out of memory and was killed."
		return base, true

	case t == "exit" || s == "exited":
		base.Kind = event.KindMachineExit
		base.Severity = event.SeverityWarning
		base.Title = fmt.Sprintf("%s exited (%s)", m.ID, app)
		base.Detail = "Machine process exited."
		return base, true

	case strings.HasPrefix(t, "start") || s == "started" || s == "starting":
		base.Kind = event.KindMachineStarted
		base.Severity = event.SeverityInfo
		base.Title = fmt.Sprintf("%s started (%s)", m.ID, app)
		return base, true

	case strings.HasPrefix(t, "stop") || s == "stopped" || s == "stopping":
		base.Kind = event.KindMachineStopped
		base.Severity = event.SeverityWarning
		base.Title = fmt.Sprintf("%s stopped (%s)", m.ID, app)
		return base, true

	case t == "create" || t == "launch" || s == "created":
		base.Kind = event.KindMachineCreated
		base.Severity = event.SeverityInfo
		base.Title = fmt.Sprintf("%s created (%s)", m.ID, app)
		return base, true

	case t == "destroy" || s == "destroyed":
		base.Kind = event.KindMachineDestroyed
		base.Severity = event.SeverityInfo
		base.Title = fmt.Sprintf("%s destroyed (%s)", m.ID, app)
		return base, true

	case strings.Contains(t, "healthcheck") || strings.Contains(t, "health_check") || strings.Contains(t, "check"):
		if strings.Contains(s, "fail") || strings.Contains(s, "critical") || strings.Contains(s, "warn") {
			base.Kind = event.KindHealthCheckFailing
			base.Severity = event.SeverityCritical
			base.Title = fmt.Sprintf("Health check failing on %s (%s)", m.ID, app)
			return base, true
		}
		if strings.Contains(s, "pass") || strings.Contains(s, "ok") {
			base.Kind = event.KindHealthCheckPassing
			base.Severity = event.SeverityInfo
			base.Title = fmt.Sprintf("Health check passing on %s (%s)", m.ID, app)
			return base, true
		}

	case t == "restart" || strings.Contains(t, "restart"):
		base.Kind = event.KindMachineStarted
		base.Severity = event.SeverityInfo
		base.Title = fmt.Sprintf("%s restarting (%s)", m.ID, app)
		return base, true
	}

	// Unknown type/status — emit a generic machine event so we never
	// silently drop something that landed in events[]. Better to be
	// slightly noisy than to miss a state change.
	base.Kind = event.KindMachineEvent
	base.Severity = event.SeverityInfo
	base.Title = fmt.Sprintf("%s: %s/%s on %s", app, e.Type, e.Status, m.ID)
	return base, true
}

func (p *Poller) emit(ev event.Event) {
	select {
	case p.Out <- ev:
	default:
		p.Logger.Warn("event channel full, dropping", "kind", ev.Kind, "app", ev.App)
	}
}
