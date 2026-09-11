package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type SessionsExecOptions struct {
	Repository, Thread  string
	Metadata, MCPScopes []string
	MaxSpendUSD         float64
	PersistSessionID    func(context.Context, string) error
}
type sessionsBackend struct{ cfg Config }

func validSessionsRepository(repository string) bool {
	name, ref, ok := strings.Cut(repository, "@")
	if !ok || strings.Contains(ref, "@") || !validSessionsCheckoutRef(ref) {
		return false
	}
	parts := strings.Split(name, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && !strings.ContainsAny(name, "@ \t\r\n")
}

func validSessionsCheckoutRef(ref string) bool {
	if ref == "" || ref == "@" || strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") || strings.Contains(ref, "//") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	return true
}

// discoverSessionsModels asks the configured DevTools CLI for AgentGateway's
// live model selectors. Selectors are intentionally preserved verbatim:
// AgentGateway, not Multica, owns their meaning and routing.
func discoverSessionsModels(ctx context.Context, runtimeCmd Command) ([]Model, error) {
	if runtimeCmd.Path == "" {
		runtimeCmd.Path = "devtools"
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := runtimeCmd.exec(runCtx, "agent", "models")
	hideAgentWindow(cmd)
	stdout, err := outputOwned(cmd, runtimeCmd.logger)
	if err != nil {
		return nil, fmt.Errorf("discover Sessions models: %w", err)
	}
	return parseSessionsModels(stdout), nil
}

func parseSessionsModels(stdout []byte) []Model {
	seen := make(map[string]struct{})
	models := make([]Model, 0)
	for _, line := range strings.Split(string(stdout), "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		provider := ""
		if prefix, _, ok := strings.Cut(id, "/"); ok {
			provider = prefix
		}
		models = append(models, Model{
			ID:       id,
			Label:    id,
			Provider: provider,
			Default:  id == "devtools/standard",
		})
	}
	return models
}

func (b *sessionsBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	o := opts.Sessions
	if o == nil || !validSessionsRepository(o.Repository) || o.Thread == "" || o.MaxSpendUSD <= 0 || o.PersistSessionID == nil {
		return nil, fmt.Errorf("invalid Sessions options")
	}
	for _, s := range o.MCPScopes {
		if !strings.HasPrefix(s, "mcp:") || s == "mcp:sessions" || s == "mcp:aws" {
			return nil, fmt.Errorf("unsupported Sessions scope %q", s)
		}
	}
	args := []string{"session", "dispatch", "--runtime", "cloud", "--tool", "bubo", "--model", opts.Model, "--max-spend-usd", fmt.Sprintf("%g", o.MaxSpendUSD), "--repo", o.Repository, "--thread", o.Thread}
	for _, m := range o.Metadata {
		args = append(args, "--metadata", m)
	}
	for _, s := range o.MCPScopes {
		args = append(args, "--mcp-scope", s)
	}
	args = append(args, "--json")
	cmd := b.cfg.commandAt(b.cfg.ExecutablePath).exec(ctx, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = os.Environ()
	for k, v := range b.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, sessionsDispatchError(err)
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err = json.Unmarshal(out, &created); err != nil {
		return nil, fmt.Errorf("decode Sessions dispatch response: %w", err)
	}
	if created.SessionID == "" {
		return nil, fmt.Errorf("Sessions dispatch response has no session_id")
	}
	if err = o.PersistSessionID(ctx, created.SessionID); err != nil {
		return nil, err
	}
	messages := make(chan Message, 8)
	result := make(chan Result, 1)
	go func() {
		defer close(messages)
		defer close(result)
		// `session watch` is a long-lived JSONL stream. Decode each complete line
		// before waiting for the process to exit so the existing task-message path
		// can persist and publish useful activity while the Session is running.
		watch := b.cfg.commandAt(b.cfg.ExecutablePath).exec(ctx, "session", "watch", created.SessionID, "--json", "--since", "0")
		watch.Env = cmd.Env
		watchOut, pipeErr := watch.StdoutPipe()
		if pipeErr != nil {
			result <- Result{Status: "failed", Error: fmt.Sprintf("Sessions watch setup failed: %v", pipeErr), SessionID: created.SessionID}
			return
		}
		if err := watch.Start(); err != nil {
			result <- Result{Status: "failed", Error: fmt.Sprintf("Sessions watch start failed: %v", err), SessionID: created.SessionID}
			return
		}
		scanner := bufio.NewScanner(watchOut)
		// Watcher fields are bounded by the Sessions API, but keep enough room for
		// a long redacted input summary without accepting an unbounded line.
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		highestSeq := -1
	watchLines:
		for scanner.Scan() {
			message, seq, ok := parseSessionsWatchLine(scanner.Bytes(), highestSeq, created.SessionID)
			if !ok {
				continue
			}
			highestSeq = seq
			if message.Type == "" {
				continue
			}
			select {
			case messages <- message:
			case <-ctx.Done():
				break watchLines
			}
		}
		watchErr := watch.Wait()
		if scanErr := scanner.Err(); scanErr != nil && watchErr == nil {
			watchErr = scanErr
		}
		if ctx.Err() != nil {
			cleanupSessionsProcess(b, cmd.Env, created.SessionID)
		}
		row, getErr := getSessionsFinal(b, cmd.Env, created.SessionID)
		if getErr != nil {
			result <- Result{Status: "failed", Error: fmt.Sprintf("Sessions final read failed: %v", getErr), SessionID: created.SessionID}
			return
		}
		switch row.State {
		case "succeeded":
			result <- Result{Status: "completed", Output: row.ResultSummary, SessionID: created.SessionID}
		case "cancelled":
			result <- Result{Status: "cancelled", SessionID: created.SessionID}
		case "failed":
			result <- Result{Status: "failed", Error: row.ResultSummary, SessionID: created.SessionID}
		default:
			errText := fmt.Sprintf("Sessions final state is nonterminal: %s", row.State)
			if watchErr != nil {
				errText += fmt.Sprintf(" (watch: %v)", watchErr)
			}
			result <- Result{Status: "failed", Error: errText, SessionID: created.SessionID}
		}
	}()
	return &Session{Messages: messages, Result: result}, nil
}

func parseSessionsWatchLine(line []byte, highestSeq int, sessionID string) (Message, int, bool) {
	var event struct {
		Type, Text, Tool, InputSummary string
		Seq                            int
	}
	if json.Unmarshal(line, &event) != nil || event.Seq <= highestSeq {
		return Message{}, highestSeq, false
	}
	switch event.Type {
	case "assistant_delta":
		return Message{Type: MessageText, Content: event.Text}, event.Seq, true
	case "tool_call_started":
		return Message{Type: MessageToolUse, Tool: event.Tool, Content: event.InputSummary}, event.Seq, true
	case "tool_call_finished":
		return Message{Type: MessageToolResult, Tool: event.Tool}, event.Seq, true
	case "state_changed":
		return Message{Type: MessageStatus, SessionID: sessionID}, event.Seq, true
	default:
		// Advance the sequence for intentionally unprojected fields such as cost
		// ticks so a duplicate replay cannot be mistaken for new activity.
		return Message{}, event.Seq, true
	}
}

type sessionsFinalRow struct {
	State         string `json:"state"`
	ResultSummary string `json:"result_summary"`
}

// sessionsDispatchError preserves a bounded CLI diagnostic when create has an
// ambiguous transport outcome. The caller must still treat it as unknown and
// obtain authoritative readback before any new create attempt.
func sessionsDispatchError(err error) error {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("Sessions dispatch outcome unknown: %w", err)
	}
	const maxDiagnosticBytes = 1024
	diagnostic := strings.TrimSpace(string(exitErr.Stderr))
	if len(diagnostic) > maxDiagnosticBytes {
		diagnostic = diagnostic[:maxDiagnosticBytes] + "…"
	}
	if diagnostic == "" {
		return fmt.Errorf("Sessions dispatch outcome unknown: %w", err)
	}
	return fmt.Errorf("Sessions dispatch outcome unknown: %w: %s", err, diagnostic)
}

func getSessionsFinal(b *sessionsBackend, env []string, id string) (sessionsFinalRow, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := b.cfg.commandAt(b.cfg.ExecutablePath).exec(ctx, "session", "get", id, "--json")
		cmd.Env = env
		out, err := cmd.Output()
		cancel()
		if err != nil {
			return sessionsFinalRow{}, err
		}
		var row sessionsFinalRow
		if err := json.Unmarshal(out, &row); err != nil {
			return sessionsFinalRow{}, fmt.Errorf("decode Sessions final response: %w", err)
		}
		if row.State == "" {
			return sessionsFinalRow{}, fmt.Errorf("Sessions final response has no state")
		}
		if row.State == "succeeded" || row.State == "failed" || row.State == "cancelled" || time.Now().After(deadline) {
			return row, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}
func cleanupSessionsProcess(b *sessionsBackend, env []string, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := b.cfg.commandAt(b.cfg.ExecutablePath).exec(ctx, "session", "cancel", id, "--json")
	cmd.Env = env
	_, _ = cmd.Output()
	_, _ = getSessionsFinal(b, env, id)
}
