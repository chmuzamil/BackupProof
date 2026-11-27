package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Stopping the agent (systemctl stop, Ctrl-C) cancels its context while it is
// waiting on a poll. That is a clean exit: Run must return nil, or systemd
// records the service as failed on every upgrade and reboot.
func TestRunStopsCleanly(t *testing.T) {
	polling := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/poll" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		select {
		case polling <- struct{}{}:
		default:
		}
		select { // a long poll with no work
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer close(release) // runs first, so Close isn't left waiting on the poll

	// Asking Docker can take up to 10 seconds on a CI runner; it isn't what's tested.
	oldDocker := dockerAvailable
	dockerAvailable = func(context.Context) bool { return false }
	t.Cleanup(func() { dockerAvailable = oldDocker })

	a := &Agent{dir: t.TempDir(), st: &State{Server: srv.URL, Name: "test"}, c: &client{base: srv.URL, http: srv.Client()}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case <-polling:
	case <-time.After(10 * time.Second):
		t.Fatal("agent never polled")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v after a requested stop, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
