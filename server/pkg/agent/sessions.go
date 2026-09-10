package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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

var immutableSessionsRepository = regexp.MustCompile(`^[^/@\s]+/[^/@\s]+@[0-9a-f]{40}$`)

func (b *sessionsBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	o := opts.Sessions
	if o == nil || !immutableSessionsRepository.MatchString(o.Repository) || o.Thread == "" || o.MaxSpendUSD <= 0 || o.PersistSessionID == nil {
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
		return nil, fmt.Errorf("Sessions dispatch outcome unknown: %w", err)
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
		watch := b.cfg.commandAt(b.cfg.ExecutablePath).exec(ctx, "session", "watch", created.SessionID, "--json", "--since", "0")
		watch.Env = cmd.Env
		watched, watchErr := watch.Output()
		if ctx.Err() != nil {
			cleanupSessionsProcess(b, cmd.Env, created.SessionID)
		}
		row, getErr := getSessionsFinal(b, cmd.Env, created.SessionID)
		if getErr != nil {
			result <- Result{Status: "failed", Error: fmt.Sprintf("Sessions final read failed: %v", getErr), SessionID: created.SessionID}
			return
		}
		highestSeq := -1
		for _, line := range strings.Split(strings.TrimSpace(string(watched)), "\n") {
			var e struct {
				Type, Text, Tool, InputSummary string
				Seq                            int
			}
			if json.Unmarshal([]byte(line), &e) != nil || e.Seq <= highestSeq {
				continue
			}
			highestSeq = e.Seq
			switch e.Type {
			case "assistant_delta":
				messages <- Message{Type: MessageText, Content: e.Text}
			case "tool_call_started":
				messages <- Message{Type: MessageToolUse, Tool: e.Tool, Content: e.InputSummary}
			case "tool_call_finished":
				messages <- Message{Type: MessageToolResult, Tool: e.Tool}
			case "state_changed":
				messages <- Message{Type: MessageStatus, SessionID: created.SessionID}
			}
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

type sessionsFinalRow struct {
	State         string `json:"state"`
	ResultSummary string `json:"result_summary"`
}

func getSessionsFinal(b *sessionsBackend, env []string, id string) (sessionsFinalRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := b.cfg.commandAt(b.cfg.ExecutablePath).exec(ctx, "session", "get", id, "--json")
	cmd.Env = env
	out, err := cmd.Output()
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
	return row, nil
}
func cleanupSessionsProcess(b *sessionsBackend, env []string, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := b.cfg.commandAt(b.cfg.ExecutablePath).exec(ctx, "session", "cancel", id, "--json")
	cmd.Env = env
	_, _ = cmd.Output()
	_, _ = getSessionsFinal(b, env, id)
}
