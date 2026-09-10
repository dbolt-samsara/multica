package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestProbeSessionsExecutableRequiresExplicitAbsolutePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	bin := filepath.Join(t.TempDir(), "devtools")
	writeDaemonTestExecutable(t, bin, []byte("#!/bin/sh\nexit 0\n"))
	for name, value := range map[string]struct {
		value string
		want  bool
	}{
		"absent":   {"", false},
		"relative": {"devtools", false},
		"missing":  {filepath.Join(t.TempDir(), "missing"), false},
		"absolute": {bin, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MULTICA_SESSIONS_PATH", value.value)
			got, ok := probeSessionsExecutable()
			if ok != value.want {
				t.Fatalf("ok = %v, want %v (entry=%+v)", ok, value.want, got)
			}
			if ok && got.Path != bin {
				t.Fatalf("Path = %q, want %q", got.Path, bin)
			}
		})
	}
}

func TestPreflightSessionsExecutableUsesOnlyReadOnlyCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "devtools")
	writeDaemonTestExecutable(t, bin, []byte(`#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$CALLS"
case "$1 $2" in
  "auth status"|"session list") exit 0 ;;
  *) exit 64 ;;
esac
`))
	t.Setenv("CALLS", log)
	if err := preflightSessionsExecutable(context.Background(), AgentEntry{Path: bin}); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "auth status\nsession list --json --limit 1\n" {
		t.Fatalf("calls = %q", got)
	}
}

func TestPreflightSessionsExecutableRejectsRelativePath(t *testing.T) {
	if err := preflightSessionsExecutable(context.Background(), AgentEntry{Path: "devtools"}); err == nil {
		t.Fatal("relative path accepted")
	}
}

func TestDetectBuiltinRuntimesRegistersSessionsOnlyAfterPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	fx := newBatchFixture(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "devtools")
	writeDaemonTestExecutable(t, bin, []byte(`#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$CALLS"
case "$1 $2" in
  "auth status"|"session list") exit 0 ;;
  *) exit 64 ;;
esac
`))
	t.Setenv("CALLS", log)
	agents := map[string]AgentEntry{"sessions": {Path: bin}}
	fx.daemon.agentsAvailable.Store(&agents)

	runtimes, _, unavailable := fx.daemon.detectBuiltinRuntimes(context.Background())
	if len(unavailable) != 0 {
		t.Fatalf("unavailable = %#v, want none", unavailable)
	}
	if len(runtimes) != 1 || runtimes[0]["type"] != "sessions" {
		t.Fatalf("runtimes = %#v, want Sessions", runtimes)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "auth status\nsession list --json --limit 1\n" {
		t.Fatalf("preflight calls = %q", got)
	}
}

func TestDetectBuiltinRuntimesSkipsSessionsWhenPreflightFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	fx := newBatchFixture(t)
	bin := filepath.Join(t.TempDir(), "devtools")
	writeDaemonTestExecutable(t, bin, []byte(`#!/bin/sh
case "$1 $2" in
  "auth status") exit 0 ;;
  "session list") exit 1 ;;
  *) exit 64 ;;
esac
`))
	agents := map[string]AgentEntry{"sessions": {Path: bin}}
	fx.daemon.agentsAvailable.Store(&agents)

	runtimes, _, unavailable := fx.daemon.detectBuiltinRuntimes(context.Background())
	if len(runtimes) != 0 {
		t.Fatalf("runtimes = %#v, want none", runtimes)
	}
	if _, ok := unavailable["sessions"]; !ok {
		t.Fatalf("unavailable = %#v, want Sessions preflight failure", unavailable)
	}
	fx.mu.Lock()
	probes := fx.probes[bin]
	fx.mu.Unlock()
	if probes != 0 {
		t.Fatalf("version probes = %d, want 0 after preflight failure", probes)
	}
}
