package digest

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/becreacom/flyio-slack/internal/event"
	"github.com/becreacom/flyio-slack/internal/flyapi"
)

type Digester struct {
	Client *flyapi.Client
	Apps   []string
	Out    chan<- event.Event
	Logger *slog.Logger
}

type AppSummary struct {
	App           string
	StateCounts   map[string]int
	Regions       map[string]int
	LatestImage   string
	LatestDeploy  time.Time
	FailingChecks int
	Total         int
}

type Snapshot struct {
	GeneratedAt time.Time
	Apps        []AppSummary
	OverallSeverity event.Severity
}

// Run computes a snapshot of all configured apps and emits a single
// digest event.
func (d *Digester) Run(ctx context.Context) {
	snap, err := d.snapshot(ctx)
	if err != nil {
		d.Logger.Warn("digest snapshot failed", "err", err)
		return
	}
	d.Out <- event.Event{
		Kind:      event.KindDigest,
		Severity:  snap.OverallSeverity,
		Timestamp: snap.GeneratedAt,
		Title:     fmt.Sprintf("Fly.io status digest — %s", snap.GeneratedAt.UTC().Format("15:04 UTC")),
		Detail:    renderDigestText(snap),
		Payload:   snap,
	}
}

func (d *Digester) snapshot(ctx context.Context) (Snapshot, error) {
	now := time.Now()
	snap := Snapshot{GeneratedAt: now, OverallSeverity: event.SeverityInfo}

	for _, app := range d.Apps {
		machines, err := d.Client.ListMachines(ctx, app)
		if err != nil {
			d.Logger.Warn("digest list machines", "app", app, "err", err)
			continue
		}
		summary := summarize(app, machines)
		snap.Apps = append(snap.Apps, summary)
		if sev := summary.Severity(); severityRank(sev) > severityRank(snap.OverallSeverity) {
			snap.OverallSeverity = sev
		}
	}
	sort.Slice(snap.Apps, func(i, j int) bool { return snap.Apps[i].App < snap.Apps[j].App })
	return snap, nil
}

func summarize(app string, machines []flyapi.Machine) AppSummary {
	s := AppSummary{
		App:         app,
		StateCounts: map[string]int{},
		Regions:     map[string]int{},
		Total:       len(machines),
	}
	for _, m := range machines {
		s.StateCounts[m.State]++
		if m.Region != "" {
			s.Regions[m.Region]++
		}
		ref := m.ImageRef.String()
		if ref == "" {
			ref = m.Config.Image
		}
		if ref != "" {
			s.LatestImage = ref
		}
		for _, c := range m.Checks {
			if !isHealthy(c.Status) {
				s.FailingChecks++
			}
		}
		// best-effort: pick the most recent create/launch event timestamp
		for _, e := range m.Events {
			lt := strings.ToLower(e.Type)
			if lt == "create" || lt == "launch" || lt == "start" {
				if t := e.Time(); t.After(s.LatestDeploy) {
					s.LatestDeploy = t
				}
			}
		}
	}
	return s
}

func isHealthy(status string) bool {
	s := strings.ToLower(status)
	return s == "passing" || s == "ok" || s == ""
}

func (s AppSummary) Severity() event.Severity {
	if s.Total == 0 {
		return event.SeverityCritical
	}
	started := s.StateCounts["started"]
	if started == 0 {
		return event.SeverityCritical
	}
	if s.FailingChecks > 0 || started < s.Total {
		return event.SeverityWarning
	}
	return event.SeverityInfo
}

func severityRank(s event.Severity) int {
	switch s {
	case event.SeverityCritical:
		return 2
	case event.SeverityWarning:
		return 1
	default:
		return 0
	}
}

func renderDigestText(snap Snapshot) string {
	var b strings.Builder
	for _, a := range snap.Apps {
		fmt.Fprintf(&b, "*%s* — %d machine(s)", a.App, a.Total)
		if a.Total > 0 {
			fmt.Fprintf(&b, ", states: %s", formatStateCounts(a.StateCounts))
		}
		if len(a.Regions) > 0 {
			fmt.Fprintf(&b, ", regions: %s", formatStringIntMap(a.Regions))
		}
		if a.FailingChecks > 0 {
			fmt.Fprintf(&b, ", *%d failing check(s)*", a.FailingChecks)
		}
		if !a.LatestDeploy.IsZero() {
			fmt.Fprintf(&b, ", last activity: %s", a.LatestDeploy.UTC().Format("2006-01-02 15:04 UTC"))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatStateCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
	}
	return strings.Join(parts, ", ")
}

func formatStringIntMap(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
