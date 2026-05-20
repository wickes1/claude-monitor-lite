package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClassifyUpdate(t *testing.T) {
	authErr := fmt.Errorf("%w: detail", ErrAuthFailed)
	transientErr := fmt.Errorf("%w: detail", ErrTransient)
	unknownErr := errors.New("something unexpected")

	cases := []struct {
		name  string
		err   error
		count int
		want  updateAction
	}{
		{"auth failure shows login immediately", authErr, 1, actionShowAuth},
		{"auth failure outranks transient count", authErr, 99, actionShowAuth},
		{"first transient keeps stale data", transientErr, 1, actionKeepStale},
		{"transient at tolerance boundary keeps stale", transientErr, maxTransientErrors, actionKeepStale},
		{"transient past tolerance shows error", transientErr, maxTransientErrors + 1, actionShowError},
		{"unknown error shows error immediately", unknownErr, 1, actionShowError},
	}

	for _, tc := range cases {
		if got := classifyUpdate(tc.err, tc.count); got != tc.want {
			t.Errorf("%s: classifyUpdate(%v, %d) = %v, want %v",
				tc.name, tc.err, tc.count, got, tc.want)
		}
	}
}

func TestIsOurProcess(t *testing.T) {
	// The running test binary is "claude-monitor-lite.test" — our process.
	if !isOurProcess(os.Getpid()) {
		t.Error("isOurProcess(self) = false, want true")
	}
	if isOurProcess(-1) {
		t.Error("isOurProcess(-1) = true, want false")
	}
	if isOurProcess(0) {
		t.Error("isOurProcess(0) = true, want false")
	}
	// A process that is definitely not claude-monitor-lite.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to run helper process: %v", err)
	}
	if isOurProcess(cmd.Process.Pid) {
		t.Error("isOurProcess(non-claude process) = true, want false")
	}
}

func TestCreatePIDFileExclusive(t *testing.T) {
	old := pidFile
	pidFile = filepath.Join(t.TempDir(), "test.pid")
	defer func() { pidFile = old }()

	if err := createPIDFile(); err != nil {
		t.Fatalf("first createPIDFile failed: %v", err)
	}
	if err := createPIDFile(); err == nil {
		t.Error("second createPIDFile succeeded, want error (O_EXCL must refuse an existing file)")
	}
}

// A transient blip during login validation must not reject a valid key.
func TestValidateSessionRetriesTransient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/organizations") {
			_, _ = w.Write([]byte(`[{"uuid":"org-1"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":10}}`))
	}))
	defer srv.Close()

	if err := validateSession(newTestClient(srv.URL, ""), 3, 0); err != nil {
		t.Fatalf("validateSession should succeed after transient blips, got: %v", err)
	}
}

// A real auth failure must be reported immediately, without retrying.
func TestValidateSessionExpiredNoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := validateSession(newTestClient(srv.URL, ""), 3, 0)
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired, got: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d HTTP calls, want 1 (must not retry a real auth failure)", n)
	}
}

func TestValidateSessionGivesUpAfterAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := validateSession(newTestClient(srv.URL, ""), 3, 0); err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
}
