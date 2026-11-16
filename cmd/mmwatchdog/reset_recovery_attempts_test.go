package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"mmwatchdog/internal/daemon"
	"mmwatchdog/internal/incidents"
)

// reset-recovery-attempts prints one plain sentence, not the daemon's JSON.
func TestResetRecoveryAttemptsPrintsOnePlainSentence(t *testing.T) {
	socket := filepath.Join(os.TempDir(), "mmwatchdog-"+incidents.ID()+".sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"recovery_attempts_reset": true})
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	original := os.Stdout
	os.Stdout = output
	defer func() { os.Stdout = original }()
	if err := run([]string{"reset-recovery-attempts", "myapp", "--socket", socket}); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(output.Name())
	if want := "The recovery attempts of check group myapp are reset. MMWatchdog can attempt recovery actions again.\n"; string(text) != want {
		t.Fatalf("printed %q, want %q", text, want)
	}
}

// A budget with no last attempt result, such as after a reset, shows only the count.
func TestAttemptsUsedShowsNoResultAfterAReset(t *testing.T) {
	if got, want := attemptsUsed(daemon.RecoveryAttemptStatus{MaxAttempts: "3"}), "0 of 3 attempts used"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := attemptsUsed(daemon.RecoveryAttemptStatus{AttemptsUsed: 1, MaxAttempts: "3", LastResult: "restart requested"}), "1 of 3 attempts used; restart requested"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
