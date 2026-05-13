package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/benjbdev/flyio-slack-notifier/internal/event"
	"github.com/benjbdev/flyio-slack-notifier/internal/flyapi"
)

const metaImageRef = "image_ref"

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

	crashes  *crashTracker
	capacity *capacityTracker
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
		crashes:   newCrashTracker(defaultCrashLoopThreshold, defaultCrashLoopWindow, defaultCrashLoopCooldown),
		capacity:  newCapacityTracker(),
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
	var wg sync.WaitGroup
	for _, app := range p.Apps {
		wg.Add(1)
		go func(app string) {
			defer wg.Done()
			if err := p.pollApp(ctx, app); err != nil {
				p.Logger.Warn("poll app failed", "app", app, "err", err)
			}
		}(app)
	}
	wg.Wait()
	return nil
}

func (p *Poller) pollApp(ctx context.Context, app string) error {
	machines, err := p.Client.ListMachines(ctx, app)
	if err != nil {
		return err
	}

	// Compute deploy-in-progress BEFORE detectDeploy updates the
	// stored image_ref — otherwise the last poll of a deploy (when
	// dominance has just flipped) would look like steady state and
	// we'd fire a stray capacity-restored on the way out.
	deploying := p.deployInProgress(app, machines)

	p.detectDeploy(app, machines)
	p.observeCapacity(app, machines, deploying)

	for _, m := range machines {
		if err := p.processMachineEvents(app, m); err != nil {
			p.Logger.Warn("process machine events", "app", app, "machine", m.ID, "err", err)
		}
	}
	return nil
}

// deployInProgress reports whether this app appears to be mid-deploy:
// machines have a mix of image_refs, OR any machine's image_ref differs
// from the dominant image we last persisted. This is a heuristic — Fly
// doesn't expose an explicit deploy state — but it's reliable enough to
// gate capacity alerts so a rolling deploy doesn't masquerade as an
// outage.
//
// Returns false when no baseline is recorded yet (first ever poll of an
// app); in that case detectDeploy is about to record one for the first
// time and there's nothing to compare against.
func (p *Poller) deployInProgress(app string, machines []flyapi.Machine) bool {
	refs := map[string]struct{}{}
	for _, m := range machines {
		ref := m.ImageRef.String()
		if ref == "" {
			ref = m.Config.Image
		}
		if ref != "" {
			refs[ref] = struct{}{}
		}
	}
	if len(refs) > 1 {
		return true
	}
	stored, _ := p.Store.GetMeta(app, metaImageRef)
	if stored == "" {
		return false
	}
	for ref := range refs {
		if ref != stored {
			return true
		}
	}
	return false
}

func (p *Poller) observeCapacity(app string, machines []flyapi.Machine, deploying bool) {
	running := 0
	for _, m := range machines {
		if strings.EqualFold(m.State, "started") {
			running++
		}
	}
	if p.bootstrap {
		p.capacity.seed(app, running)
		return
	}
	if ev, ok := p.capacity.observe(app, running, deploying, time.Now()); ok {
		p.emit(ev)
	}
}

func (p *Poller) detectDeploy(app string, machines []flyapi.Machine) {
	current := uniformImage(machines)
	if current == "" {
		// Mixed images across machines: deploy in flight, not yet
		// converged. Wait — emitting now (or worse, updating the
		// stored ref) would race the rolling cycle and flip back and
		// forth as machines swap one by one.
		return
	}

	prevImage, _ := p.Store.GetMeta(app, metaImageRef)
	if prevImage == current {
		return
	}

	firstSight := prevImage == ""
	if !firstSight && !p.bootstrap {
		p.emit(event.Event{
			Kind:      event.KindDeploy,
			Severity:  event.SeverityInfo,
			App:       app,
			Timestamp: time.Now(),
			Title:     fmt.Sprintf("%s deployed", app),
			Detail:    fmt.Sprintf("image: %s", current),
			Fields: map[string]string{
				"app":      app,
				"image":    current,
				"prev":     prevImage,
				"machines": strconv.Itoa(len(machines)),
			},
		})
	}
	_ = p.Store.SetMeta(app, metaImageRef, current)
}

// uniformImage returns the image_ref shared by every machine that has
// one, or "" if machines disagree (rolling deploy mid-flight) or none
// expose an image_ref. Returning "" deliberately stalls detectDeploy
// during transitional states: emitting "deployed" partway through a
// roll-out would race the cycle and flip back and forth as machines
// swap one by one, and would also update the stored cursor early
// enough that deployInProgress would lose visibility on the remaining
// legs of the deploy.
func uniformImage(machines []flyapi.Machine) string {
	var ref string
	for _, m := range machines {
		r := m.ImageRef.String()
		if r == "" {
			r = m.Config.Image
		}
		if r == "" {
			continue
		}
		if ref == "" {
			ref = r
		} else if ref != r {
			return ""
		}
	}
	return ref
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
		ev, ok := mapMachineEvent(app, m, e)
		if !ok {
			continue
		}
		if ev.Kind == event.KindMachineOOM || ev.Kind == event.KindMachineCrashed {
			// Snapshot cooldown state BEFORE recording this crash.
			// If a loop alert already fired and we're still inside
			// the cooldown, suppress the per-crash message — the
			// loop alert covers it. observe() still records the
			// event so the count stays accurate across windows.
			suppress := p.crashes.inCooldown(app, m.ID, ev.Timestamp)
			loop, looping := p.crashes.observe(app, m.ID, m.Region, ev.Timestamp)
			if !suppress {
				p.emit(ev)
			}
			if looping {
				p.emit(loop)
			}
			continue
		}
		p.emit(ev)
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

// mapMachineEvent translates a raw Fly machine event into a Slack-bound
// alert. It deliberately emits ONLY for events that carry actionable
// signal (crashes, OOMs, failing health checks). Routine lifecycle
// chatter — start/restart/launch/update/destroy — is dropped because:
//
//   - deploys are already announced once via KindDeploy (see detectDeploy),
//   - a machine that comes up and stays up is a non-event,
//   - a machine that goes down and stays down is caught by the
//     capacity tracker's degraded alert + re-alert,
//   - a machine that crashes is caught by the exit-payload branch below,
//     and a sustained loop is folded into KindCrashLoop.
//
// Anything not in the allowlist returns (zero, false) and is silently
// dropped. The poller still logs every event at INFO via slog, so the
// runtime logs remain a complete audit trail; only the Slack stream is
// filtered.
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
		// Legacy explicit-OOM-in-type/status. Kept for safety; the
		// payload-aware path below catches the real Fly shape where
		// type=exit and OOM lives in request.exit_event.oom_killed.
		base.Kind = event.KindMachineOOM
		base.Severity = event.SeverityCritical
		base.Title = fmt.Sprintf("%s OOM-killed (%s)", m.ID, app)
		base.Detail = "Machine ran out of memory and was killed."
		return base, true

	case t == "exit" || s == "exited":
		ex, ok := e.ParseExit()
		if !ok {
			// No exit payload → can't tell crash from clean shutdown
			// from deploy-driven stop. Stay silent rather than emit a
			// noisy "exited" warning that might just be a redeploy.
			return event.Event{}, false
		}
		switch {
		case ex.OOMKilled:
			base.Kind = event.KindMachineOOM
			base.Severity = event.SeverityCritical
			base.Title = fmt.Sprintf("%s OOM-killed (%s)", m.ID, app)
			base.Detail = fmt.Sprintf(
				"Machine ran out of memory and was killed (exit_code=%d, guest_signal=%d).",
				ex.ExitCode, ex.GuestSignal,
			)
			base.Fields["exit_code"] = strconv.Itoa(ex.ExitCode)
			base.Fields["oom_killed"] = "true"
			return base, true
		case ex.ExitCode != 0 && !ex.RequestedStop:
			base.Kind = event.KindMachineCrashed
			base.Severity = event.SeverityCritical
			base.Title = fmt.Sprintf("%s crashed (%s)", m.ID, app)
			base.Detail = fmt.Sprintf(
				"Machine exited with non-zero status (exit_code=%d, guest_signal=%d).",
				ex.ExitCode, ex.GuestSignal,
			)
			base.Fields["exit_code"] = strconv.Itoa(ex.ExitCode)
			base.Fields["guest_signal"] = strconv.Itoa(ex.GuestSignal)
			return base, true
		}
		// Clean exit (exit_code=0 or requested_stop=true). Silent —
		// indistinguishable from a deploy/replace/scale stop, so we
		// don't classify it as a process failure.
		return event.Event{}, false

	case strings.Contains(t, "healthcheck") || strings.Contains(t, "health_check") || strings.Contains(t, "check"):
		if strings.Contains(s, "fail") || strings.Contains(s, "critical") || strings.Contains(s, "warn") {
			base.Kind = event.KindHealthCheckFailing
			base.Severity = event.SeverityCritical
			base.Title = fmt.Sprintf("Health check failing on %s (%s)", m.ID, app)
			return base, true
		}
		// Healthcheck passing/recovery → silent. Capacity + crash
		// alerts already cover the inverse signal.
		return event.Event{}, false
	}

	// start/restart/launch/update/stop/destroy and any other lifecycle
	// transitions are intentionally silent.
	return event.Event{}, false
}

func (p *Poller) emit(ev event.Event) {
	select {
	case p.Out <- ev:
	default:
		p.Logger.Warn("event channel full, dropping", "kind", ev.Kind, "app", ev.App)
	}
}
