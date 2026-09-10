package daemon

import (
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
