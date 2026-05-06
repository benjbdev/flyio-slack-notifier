package flyapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const fixtureMachines = `
[
  {
    "id": "d8d8d8d8d8d8d8",
    "name": "frosty-dawn-123",
    "state": "started",
    "region": "cdg",
    "instance_id": "01H...",
    "image_ref": {
      "registry": "registry.fly.io",
      "repository": "api-prod",
      "tag": "deployment-01H",
      "digest": "sha256:abcdef"
    },
    "config": { "image": "registry.fly.io/api-prod:deployment-01H" },
    "events": [
      { "id": "e1", "type": "create",  "status": "created",  "source": "user",     "timestamp": 1700000000000 },
      { "id": "e2", "type": "start",   "status": "started",  "source": "flyd",     "timestamp": 1700000010000 },
      { "id": "e3", "type": "exit",    "status": "exited",   "source": "flyd",     "timestamp": 1700000020000 }
    ],
    "checks": [
      { "name": "http", "status": "passing" }
    ]
  }
]`

func TestListMachinesParsesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tk_test" {
			t.Errorf("auth header = %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/apps/api-prod/machines") {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fixtureMachines))
	}))
	defer srv.Close()

	c := New(srv.URL, "tk_test")
	machines, err := c.ListMachines(context.Background(), "api-prod")
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("len = %d", len(machines))
	}
	m := machines[0]
	if m.ID != "d8d8d8d8d8d8d8" || m.State != "started" || m.Region != "cdg" {
		t.Errorf("unexpected machine: %+v", m)
	}
	if m.ImageRef.String() != "api-prod:deployment-01H@sha256:abcdef" {
		t.Errorf("image ref string = %q", m.ImageRef.String())
	}
	if len(m.Events) != 3 {
		t.Fatalf("events len = %d", len(m.Events))
	}
	if m.Events[0].Type != "create" {
		t.Errorf("first event type = %q", m.Events[0].Type)
	}
}

func TestListMachines404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "tk_test")
	if _, err := c.ListMachines(context.Background(), "missing"); err == nil {
		t.Errorf("expected 404 error")
	}
}

func TestParseExit(t *testing.T) {
	cases := []struct {
		name      string
		request   string
		wantOK    bool
		wantOOM   bool
		wantCode  int
		wantStop  bool
	}{
		{
			name:    "oom-killed",
			request: `{"exit_event":{"exit_code":137,"guest_signal":-1,"oom_killed":true,"requested_stop":false}}`,
			wantOK:  true, wantOOM: true, wantCode: 137,
		},
		{
			name:    "crash non-zero exit",
			request: `{"exit_event":{"exit_code":139,"guest_signal":11,"oom_killed":false,"requested_stop":false}}`,
			wantOK:  true, wantCode: 139,
		},
		{
			name:    "requested stop",
			request: `{"exit_event":{"exit_code":0,"oom_killed":false,"requested_stop":true}}`,
			wantOK:  true, wantStop: true,
		},
		{name: "no request payload", request: "", wantOK: false},
		{name: "missing exit_event", request: `{"foo":"bar"}`, wantOK: false},
		{name: "malformed json", request: `{not json`, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := MachineEvent{Request: []byte(tc.request)}
			got, ok := ev.ParseExit()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.OOMKilled != tc.wantOOM {
				t.Errorf("oom_killed = %v, want %v", got.OOMKilled, tc.wantOOM)
			}
			if got.ExitCode != tc.wantCode {
				t.Errorf("exit_code = %d, want %d", got.ExitCode, tc.wantCode)
			}
			if got.RequestedStop != tc.wantStop {
				t.Errorf("requested_stop = %v, want %v", got.RequestedStop, tc.wantStop)
			}
		})
	}
}

func TestListMachines500BodyIncluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tk_test")
	_, err := c.ListMachines(context.Background(), "any")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected error with body; got %v", err)
	}
}
