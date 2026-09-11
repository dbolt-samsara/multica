package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeFakeDevtools(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "devtools")
	body := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_CALLS"
if [ "$1 $2" = "session dispatch" ]; then
  cat > "$FAKE_PROMPT"
  printf '%s\n' '{"session_id":"sess-123"}'
elif [ "$1 $2" = "session watch" ]; then
  printf '%s\n' \
    '{"type":"assistant_delta","seq":1,"session_id":"sess-123","text":"hello"}' \
    '{"type":"assistant_delta","seq":1,"session_id":"sess-123","text":"duplicate"}' \
    '{"type":"tool_call_started","seq":2,"session_id":"sess-123","tool":"bash","op_id":"op-1","input_summary":"run tests"}' \
    '{"type":"tool_call_finished","seq":3,"session_id":"sess-123","tool":"bash","op_id":"op-1","ok":true,"duration_ms":25}' \
    '{"type":"state_changed","seq":4,"session_id":"sess-123","from":"running","to":"succeeded"}'
elif [ "$1 $2" = "session get" ]; then
  printf '%s\n' '{"session_id":"sess-123","state":"succeeded","result_summary":"finished"}'
elif [ "$1 $2" = "session cancel" ]; then
  printf '%s\n' '{"session_id":"sess-123","state":"cancelled"}'
else
  exit 64
fi
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake devtools: %v", err)
	}
	t.Setenv("FAKE_CALLS", logPath)
	t.Setenv("FAKE_PROMPT", filepath.Join(dir, "prompt.txt"))
	return script, logPath
}

func TestResolveBackendSessionsIsStandalone(t *testing.T) {
	t.Parallel()
	if IsSupportedType("sessions") {
		t.Fatal("sessions must not become a custom runtime protocol family")
	}
	if _, err := New("sessions", Config{}); err == nil {
		t.Fatal("New(sessions) succeeded; standalone runtime must use ResolveBackend")
	}
	backend, err := ResolveBackend("sessions", Config{ExecutablePath: "/missing/devtools", Logger: slog.Default()})
	if err != nil {
		t.Fatalf("ResolveBackend(sessions): %v", err)
	}
	if _, ok := backend.(*sessionsBackend); !ok {
		t.Fatalf("backend type = %T, want *sessionsBackend", backend)
	}
}

func TestListModelsSessionsUsesConfiguredDevtoolsCatalog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "devtools")
	body := `#!/bin/sh
set -eu
printf '%s\n' "$*" > "$FAKE_CALLS"
printf '%s\n' 'devtools/fast' 'openai/gpt-5.6' 'devtools/standard' 'openai/gpt-5.6' ''
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CALLS", logPath)
	catalog, err := ListModels(t.Context(), "sessions", NewCommand(script, nil))
	if err != nil {
		t.Fatalf("ListModels(sessions): %v", err)
	}
	want := []Model{
		{ID: "devtools/fast", Label: "devtools/fast", Provider: "devtools"},
		{ID: "openai/gpt-5.6", Label: "openai/gpt-5.6", Provider: "openai"},
		{ID: "devtools/standard", Label: "devtools/standard", Provider: "devtools", Default: true},
	}
	if !reflect.DeepEqual(catalog.Models, want) {
		t.Fatalf("models = %+v, want %+v", catalog.Models, want)
	}
	call, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(call)) != "agent models" {
		t.Fatalf("devtools args = %q, want agent models", call)
	}
}

func TestSessionsBackendPassesConfiguredModelVerbatim(t *testing.T) {
	script, logPath := writeFakeDevtools(t)
	backend, err := ResolveBackend("sessions", Config{ExecutablePath: script, Env: map[string]string{
		"FAKE_CALLS": os.Getenv("FAKE_CALLS"), "FAKE_PROMPT": os.Getenv("FAKE_PROMPT"),
	}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	session, err := backend.Execute(t.Context(), "prompt", ExecOptions{Model: "openai/gpt-5.6", Sessions: &SessionsExecOptions{
		Repository: "samsara-dev/example@main", Thread: "multica:model-verbatim",
		MCPScopes: []string{"mcp:github"}, MaxSpendUSD: 1,
		PersistSessionID: func(context.Context, string) error { return nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range session.Messages {
	}
	<-session.Result
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Split(strings.TrimSpace(string(calls)), "\n")[0]
	if !strings.Contains(first, "--model openai/gpt-5.6") {
		t.Fatalf("dispatch did not preserve model selector: %s", first)
	}
}

func TestSessionsBackendDispatchWatchAndGet(t *testing.T) {
	script, logPath := writeFakeDevtools(t)
	var pinned string
	backend, err := ResolveBackend("sessions", Config{
		ExecutablePath: script,
		Env: map[string]string{
			"FAKE_CALLS":  os.Getenv("FAKE_CALLS"),
			"FAKE_PROMPT": os.Getenv("FAKE_PROMPT"),
		},
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("ResolveBackend: %v", err)
	}
	session, err := backend.Execute(t.Context(), "do the work\n", ExecOptions{
		Model: "devtools/standard",
		Sessions: &SessionsExecOptions{
			Repository:  "samsara-dev/example@0123456789abcdef0123456789abcdef01234567",
			Thread:      "multica:task-1",
			Metadata:    []string{"multica_task_id=task-1", "multica_issue_id=issue-1"},
			MCPScopes:   []string{"mcp:github", "mcp:github:write"},
			MaxSpendUSD: 3.5,
			PersistSessionID: func(_ context.Context, id string) error {
				pinned = id
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var messages []Message
	for message := range session.Messages {
		messages = append(messages, message)
	}
	result := <-session.Result
	if result.Status != "completed" || result.Output != "finished" || result.SessionID != "sess-123" {
		t.Fatalf("result = %+v", result)
	}
	if pinned != "sess-123" {
		t.Fatalf("pinned session = %q", pinned)
	}
	if len(messages) != 4 || messages[0].Type != MessageText || messages[1].Type != MessageToolUse || messages[2].Type != MessageToolResult || messages[3].Type != MessageStatus {
		t.Fatalf("messages = %+v", messages)
	}
	prompt, err := os.ReadFile(os.Getenv("FAKE_PROMPT"))
	if err != nil || string(prompt) != "do the work\n" {
		t.Fatalf("prompt = %q, err=%v", prompt, err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "session dispatch ") || lines[1] != "session watch sess-123 --json --since 0" || lines[2] != "session get sess-123 --json" {
		t.Fatalf("calls = %q", lines)
	}
	for _, required := range []string{"--runtime cloud", "--tool bubo", "--model devtools/standard", "--max-spend-usd 3.5", "--repo samsara-dev/example@0123456789abcdef0123456789abcdef01234567", "--thread multica:task-1", "--mcp-scope mcp:github", "--mcp-scope mcp:github:write", "--json"} {
		if !strings.Contains(lines[0], required) {
			t.Errorf("dispatch missing %q: %s", required, lines[0])
		}
	}
	if slices.Contains(strings.Fields(lines[0]), "do the work") {
		t.Fatalf("prompt leaked into argv: %s", lines[0])
	}
}

func TestSessionsBackendDispatchPreservesBoundedStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "devtools")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
printf '%s\\n' 'gateway TLS handshake timeout' >&2
exit 1
`), 0o755); err != nil {
		t.Fatal(err)
	}
	backend, err := ResolveBackend("sessions", Config{ExecutablePath: script, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Execute(t.Context(), "prompt", ExecOptions{Model: "devtools/standard", Sessions: &SessionsExecOptions{
		Repository: "samsara-dev/example@0123456789abcdef0123456789abcdef01234567", Thread: "multica:task-stderr",
		MCPScopes: []string{"mcp:github"}, MaxSpendUSD: 1,
		PersistSessionID: func(context.Context, string) error { return nil },
	}})
	if err == nil || !strings.Contains(err.Error(), "gateway TLS handshake timeout") {
		t.Fatalf("dispatch error = %v, want bounded stderr", err)
	}
}

func TestSessionsBackendAllowsSupportedMainBranch(t *testing.T) {
	script, _ := writeFakeDevtools(t)
	backend, err := ResolveBackend("sessions", Config{ExecutablePath: script, Env: map[string]string{
		"FAKE_CALLS": os.Getenv("FAKE_CALLS"), "FAKE_PROMPT": os.Getenv("FAKE_PROMPT"),
	}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Execute(t.Context(), "prompt", ExecOptions{Model: "devtools/standard", Sessions: &SessionsExecOptions{
		Repository: "samsara-dev/devbox-client@main", Thread: "multica:task-main",
		MCPScopes: []string{"mcp:github"}, MaxSpendUSD: 1,
		PersistSessionID: func(context.Context, string) error { return nil },
	}})
	if err != nil {
		t.Fatalf("Execute(main): %v", err)
	}
}

func TestSessionsBackendRejectsUnsafeOptions(t *testing.T) {
	backend := &sessionsBackend{cfg: Config{ExecutablePath: "/missing/devtools", Logger: slog.Default()}}
	base := SessionsExecOptions{
		Repository:       "samsara-dev/example@0123456789abcdef0123456789abcdef01234567",
		Thread:           "multica:task-1",
		MCPScopes:        []string{"mcp:github"},
		MaxSpendUSD:      1,
		PersistSessionID: func(context.Context, string) error { return nil },
	}
	cases := map[string]SessionsExecOptions{
		"branch ref":        func() SessionsExecOptions { v := base; v.Repository = "samsara-dev/example@main"; return v }(),
		"missing spend":     func() SessionsExecOptions { v := base; v.MaxSpendUSD = 0; return v }(),
		"missing pin":       func() SessionsExecOptions { v := base; v.PersistSessionID = nil; return v }(),
		"unsupported scope": func() SessionsExecOptions { v := base; v.MCPScopes = []string{"mcp:sessions"}; return v }(),
	}
	for name, options := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := backend.Execute(ctx, "prompt", ExecOptions{Model: "devtools/standard", Sessions: &options}); err == nil {
				t.Fatal("Execute succeeded")
			}
		})
	}
}

func TestSessionsBackendCancellationRequiresTerminalCancelledReadback(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := filepath.Join(dir, "devtools")
	body := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_CALLS"
case "$1 $2" in
  "session dispatch") cat > "$FAKE_PROMPT"; printf '%s\n' '{"session_id":"sess-cancel"}' ;;
  "session watch") while :; do :; done ;;
  "session cancel") printf '%s\n' '{"session_id":"sess-cancel","state":"cancelled"}' ;;
  "session get") printf '%s\n' '{"session_id":"sess-cancel","state":"cancelled"}' ;;
  *) exit 64 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CALLS", logPath)
	t.Setenv("FAKE_PROMPT", filepath.Join(dir, "prompt.txt"))
	backend, err := ResolveBackend("sessions", Config{ExecutablePath: script, Env: map[string]string{
		"FAKE_CALLS": os.Getenv("FAKE_CALLS"), "FAKE_PROMPT": os.Getenv("FAKE_PROMPT"),
	}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var pinned string
	session, err := backend.Execute(ctx, "prompt", ExecOptions{Model: "devtools/standard", Sessions: &SessionsExecOptions{
		Repository: "samsara-dev/example@0123456789abcdef0123456789abcdef01234567", Thread: "multica:task-cancel",
		MCPScopes: []string{"mcp:github"}, MaxSpendUSD: 1,
		PersistSessionID: func(_ context.Context, id string) error { pinned = id; return nil },
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Execute returns only after the session ID is pinned; cancellation after
	// this point must cancel remotely and use a final GET, not watcher exit.
	if pinned != "sess-cancel" {
		t.Fatalf("pinned = %q", pinned)
	}
	deadline := time.Now().Add(time.Second)
	for {
		calls, readErr := os.ReadFile(logPath)
		if readErr == nil && strings.Contains(string(calls), "session watch sess-cancel") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch did not start; calls=%q, err=%v", calls, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	for range session.Messages {
	}
	result := <-session.Result
	if result.Status != "cancelled" || result.SessionID != "sess-cancel" {
		t.Fatalf("result = %+v", result)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(calls)), "\n")
	wantPrefix := []string{"session dispatch", "session watch sess-cancel", "session cancel sess-cancel", "session get sess-cancel", "session get sess-cancel"}
	if len(got) != len(wantPrefix) {
		t.Fatalf("calls = %q, want %d calls", got, len(wantPrefix))
	}
	for i, want := range wantPrefix {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("calls[%d] = %q, want prefix %q", i, got[i], want)
		}
	}
}

func TestSessionsBackendCancellationBeforePinHasNoRemoteCleanupConfirmation(t *testing.T) {
	script, _ := writeFakeDevtools(t)
	backend, err := ResolveBackend("sessions", Config{ExecutablePath: script, Env: map[string]string{
		"FAKE_CALLS": os.Getenv("FAKE_CALLS"), "FAKE_PROMPT": os.Getenv("FAKE_PROMPT"),
	}, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	pinned := false
	_, err = backend.Execute(ctx, "prompt", ExecOptions{Model: "devtools/standard", Sessions: &SessionsExecOptions{
		Repository: "samsara-dev/example@0123456789abcdef0123456789abcdef01234567", Thread: "multica:task-before-pin",
		MCPScopes: []string{"mcp:github"}, MaxSpendUSD: 1,
		PersistSessionID: func(context.Context, string) error { pinned = true; return nil },
	}})
	if err == nil {
		t.Fatal("Execute succeeded after cancellation")
	}
	if pinned {
		t.Fatal("cancelled dispatch must not claim a pinned remote Session ID")
	}
}
