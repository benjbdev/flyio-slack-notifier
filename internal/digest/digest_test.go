package digest

import (
	"github.com/benjbdev/flyio-slack-notifier/internal/event"
	"github.com/benjbdev/flyio-slack-notifier/internal/flyapi"
	"testing"
)

func TestSummarize(t *testing.T) {
	machines := []flyapi.Machine{
		{ID: "m1", State: "started", Region: "cdg", ImageRef: flyapi.ImageRef{Repository: "api", Tag: "v1"}, Checks: []flyapi.CheckStatus{{Name: "http", Status: "passing"}}},
		{ID: "m2", State: "started", Region: "cdg", ImageRef: flyapi.ImageRef{Repository: "api", Tag: "v1"}, Checks: []flyapi.CheckStatus{{Name: "http", Status: "passing"}}},
		{ID: "m3", State: "stopped", Region: "ord", ImageRef: flyapi.ImageRef{Repository: "api", Tag: "v1"}},
	}
	s := summarize("api", machines)
	if s.Total != 3 {
		t.Errorf("Total = %d", s.Total)
	}
	if s.StateCounts["started"] != 2 || s.StateCounts["stopped"] != 1 {
		t.Errorf("StateCounts = %+v", s.StateCounts)
	}
	if s.Regions["cdg"] != 2 || s.Regions["ord"] != 1 {
		t.Errorf("Regions = %+v", s.Regions)
	}
	if s.LatestImage != "api:v1" {
		t.Errorf("LatestImage = %q", s.LatestImage)
	}
	if s.FailingChecks != 0 {
		t.Errorf("FailingChecks = %d", s.FailingChecks)
	}
	if got := s.Severity(); got != event.SeverityWarning {
		t.Errorf("Severity = %q, want warning (some stopped)", got)
	}
}

func TestSummarizeAllHealthy(t *testing.T) {
	machines := []flyapi.Machine{
		{ID: "m1", State: "started", Region: "cdg", ImageRef: flyapi.ImageRef{Repository: "api", Tag: "v1"}},
	}
	s := summarize("api", machines)
	if got := s.Severity(); got != event.SeverityInfo {
		t.Errorf("Severity = %q, want info", got)
	}
}

func TestSummarizeAllDown(t *testing.T) {
	machines := []flyapi.Machine{
		{ID: "m1", State: "stopped"},
		{ID: "m2", State: "stopped"},
	}
	s := summarize("api", machines)
	if got := s.Severity(); got != event.SeverityCritical {
		t.Errorf("Severity = %q, want critical", got)
	}
}

func TestSummarizeFailingChecks(t *testing.T) {
	machines := []flyapi.Machine{
		{
			ID: "m1", State: "started",
			Checks: []flyapi.CheckStatus{
				{Name: "http", Status: "passing"},
				{Name: "tcp", Status: "critical"},
			},
		},
	}
	s := summarize("api", machines)
	if s.FailingChecks != 1 {
		t.Errorf("FailingChecks = %d", s.FailingChecks)
	}
	if got := s.Severity(); got != event.SeverityWarning {
		t.Errorf("Severity = %q, want warning", got)
	}
}

func TestSummarizeNoMachines(t *testing.T) {
	s := summarize("api", nil)
	if s.Total != 0 {
		t.Errorf("Total = %d", s.Total)
	}
	if got := s.Severity(); got != event.SeverityCritical {
		t.Errorf("Severity for empty = %q, want critical", got)
	}
}
