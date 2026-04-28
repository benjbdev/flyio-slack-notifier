package slack

import (
	"fmt"
	"sort"
	"strings"

	"github.com/benjbdev/flyio-slack-notifier/internal/digest"
	"github.com/benjbdev/flyio-slack-notifier/internal/event"
)

// Message is the JSON payload posted to a Slack incoming webhook.
type Message struct {
	Text        string  `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment uses Slack's "secondary attachment" structure which still
// supports the colored sidebar that Block Kit messages alone don't
// expose. Each attachment carries its own blocks.
type Attachment struct {
	Color  string  `json:"color,omitempty"`
	Blocks []Block `json:"blocks,omitempty"`
}

type Block struct {
	Type     string `json:"type"`
	Text     *Text  `json:"text,omitempty"`
	Fields   []Text `json:"fields,omitempty"`
	Elements []Text `json:"elements,omitempty"`
}

type Text struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const (
	colorInfo     = "#36a64f" // green
	colorWarning  = "#daa038" // amber
	colorCritical = "#cc0000" // red
)

func severityColor(s event.Severity) string {
	switch s {
	case event.SeverityCritical:
		return colorCritical
	case event.SeverityWarning:
		return colorWarning
	default:
		return colorInfo
	}
}

// FormatEvent converts a normalized Event into a Slack webhook payload.
func FormatEvent(e event.Event) Message {
	if e.Kind == event.KindDigest {
		return formatDigest(e)
	}
	return formatAlert(e)
}

func formatAlert(e event.Event) Message {
	header := &Text{Type: "mrkdwn", Text: "*" + emoji(e) + " " + escapeMarkdown(e.Title) + "*"}
	blocks := []Block{{Type: "section", Text: header}}

	if e.Detail != "" {
		blocks = append(blocks, Block{
			Type: "section",
			Text: &Text{Type: "mrkdwn", Text: escapeMarkdown(e.Detail)},
		})
	}

	if fields := orderedFieldBlocks(e); len(fields) > 0 {
		blocks = append(blocks, Block{Type: "section", Fields: fields})
	}

	if link := dashboardLink(e); link != "" {
		blocks = append(blocks, Block{
			Type:     "context",
			Elements: []Text{{Type: "mrkdwn", Text: link}},
		})
	}

	return Message{
		Text: e.Title, // fallback for notifications
		Attachments: []Attachment{{
			Color:  severityColor(e.Severity),
			Blocks: blocks,
		}},
	}
}

func emoji(e event.Event) string {
	switch e.Kind {
	case event.KindDeploy:
		return ":rocket:"
	case event.KindMachineOOM:
		return ":boom:"
	case event.KindMachineExit, event.KindMachineStopped:
		return ":octagonal_sign:"
	case event.KindMachineStarted, event.KindMachineCreated:
		return ":white_check_mark:"
	case event.KindMachineDestroyed:
		return ":wastebasket:"
	case event.KindHealthCheckFailing:
		return ":warning:"
	case event.KindHealthCheckPassing:
		return ":white_check_mark:"
	}
	switch e.Severity {
	case event.SeverityCritical:
		return ":rotating_light:"
	case event.SeverityWarning:
		return ":warning:"
	}
	return ":information_source:"
}

// orderedFieldBlocks returns the top fields of an Event in a stable order.
func orderedFieldBlocks(e event.Event) []Text {
	if len(e.Fields) == 0 {
		return nil
	}
	priority := []string{"app", "machine", "region", "image", "prev", "type", "status", "source"}

	out := make([]Text, 0, len(e.Fields))
	seen := map[string]bool{}
	for _, k := range priority {
		v, ok := e.Fields[k]
		if !ok || v == "" {
			continue
		}
		out = append(out, Text{Type: "mrkdwn", Text: fmt.Sprintf("*%s*\n%s", k, escapeMarkdown(v))})
		seen[k] = true
	}

	rest := make([]string, 0)
	for k := range e.Fields {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		v := e.Fields[k]
		if v == "" {
			continue
		}
		out = append(out, Text{Type: "mrkdwn", Text: fmt.Sprintf("*%s*\n%s", k, escapeMarkdown(v))})
	}
	// Slack caps at 10 fields per section.
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func dashboardLink(e event.Event) string {
	if e.App == "" {
		return ""
	}
	url := "https://fly.io/apps/" + e.App
	if e.MachineID != "" {
		url += "/machines/" + e.MachineID
	}
	return fmt.Sprintf("<%s|Open in Fly dashboard>", url)
}

func escapeMarkdown(s string) string {
	// Slack mrkdwn: escape <, >, & per docs.
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func formatDigest(e event.Event) Message {
	snap, ok := e.Payload.(digest.Snapshot)
	if !ok {
		return formatAlert(e)
	}

	header := &Text{Type: "mrkdwn", Text: "*:bar_chart: " + escapeMarkdown(e.Title) + "*"}
	blocks := []Block{{Type: "section", Text: header}}

	if len(snap.Apps) == 0 {
		blocks = append(blocks, Block{
			Type: "section",
			Text: &Text{Type: "mrkdwn", Text: "_No apps configured._"},
		})
	}

	for _, a := range snap.Apps {
		blocks = append(blocks, Block{
			Type: "section",
			Text: &Text{Type: "mrkdwn", Text: renderAppSection(a)},
		})
	}

	return Message{
		Text: e.Title,
		Attachments: []Attachment{{
			Color:  severityColor(e.Severity),
			Blocks: blocks,
		}},
	}
}

func renderAppSection(a digest.AppSummary) string {
	var b strings.Builder
	emoji := ":large_green_circle:"
	switch a.Severity() {
	case event.SeverityCritical:
		emoji = ":red_circle:"
	case event.SeverityWarning:
		emoji = ":large_yellow_circle:"
	}
	fmt.Fprintf(&b, "%s *%s* — %d machine(s)", emoji, escapeMarkdown(a.App), a.Total)
	if a.Total > 0 {
		fmt.Fprintf(&b, " (%s)", digest.FormatStateCounts(a.StateCounts))
	}
	if len(a.Regions) > 0 {
		fmt.Fprintf(&b, "\n• regions: %s", digest.FormatStringIntMap(a.Regions))
	}
	if a.FailingChecks > 0 {
		fmt.Fprintf(&b, "\n• :warning: *%d failing check(s)*", a.FailingChecks)
	}
	if a.LatestImage != "" {
		fmt.Fprintf(&b, "\n• image: `%s`", escapeMarkdown(truncate(a.LatestImage, 80)))
	}
	if !a.LatestDeploy.IsZero() {
		fmt.Fprintf(&b, "\n• last activity: %s", a.LatestDeploy.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
