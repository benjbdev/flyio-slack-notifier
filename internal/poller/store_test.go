package poller

import (
	"path/filepath"
	"testing"
)

func TestStoreLastSeenRoundtrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.LastSeen("app1", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("expected 0 for unset, got %d", got)
	}

	if err := s.SetLastSeen("app1", "m1", 12345); err != nil {
		t.Fatal(err)
	}
	got, _ = s.LastSeen("app1", "m1")
	if got != 12345 {
		t.Errorf("got %d", got)
	}

	got, _ = s.LastSeen("app1", "m2")
	if got != 0 {
		t.Errorf("isolated key leaked: %d", got)
	}
}

func TestStoreMetaRoundtrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	v, err := s.GetMeta("app1", "image_ref")
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		t.Errorf("expected empty, got %q", v)
	}

	if err := s.SetMeta("app1", "image_ref", "registry.fly.io/app1:abc"); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetMeta("app1", "image_ref")
	if v != "registry.fly.io/app1:abc" {
		t.Errorf("got %q", v)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.db")

	s, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLastSeen("app1", "m1", 999); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta("app1", "k", "v"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := OpenStore(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if got, _ := s2.LastSeen("app1", "m1"); got != 999 {
		t.Errorf("LastSeen after reopen = %d", got)
	}
	if got, _ := s2.GetMeta("app1", "k"); got != "v" {
		t.Errorf("GetMeta after reopen = %q", got)
	}
}
