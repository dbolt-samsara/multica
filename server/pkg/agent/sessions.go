package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type SessionsExecOptions struct {
	Repository, Thread  string
	Metadata, MCPScopes []string
	MaxSpendUSD         float64
	PersistSessionID    func(context.Context, string) error
}
type sessionsBackend struct{ cfg Config }

func (b *sessionsBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	o := opts.Sessions
	if o == nil || !strings.Contains(o.Repository, "@") || o.MaxSpendUSD <= 0 || o.PersistSessionID == nil {
		return nil, fmt.Errorf("invalid Sessions options")
	}
	for _, s := range o.MCPScopes {
		if !strings.HasPrefix(s, "mcp:") || s == "mcp:sessions" {
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
		return nil, err
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err = json.Unmarshal(out, &created); err != nil || created.SessionID == "" {
		return nil, fmt.Errorf("invalid Sessions dispatch response: %w", err)
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
		watched, _ := watch.Output()
		for _, line := range strings.Split(strings.TrimSpace(string(watched)), "\n") {
			var e struct {
				Type, Text, Tool, InputSummary string
				Seq                            int
			}
			if json.Unmarshal([]byte(line), &e) != nil {
				continue
			}
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
		get := b.cfg.commandAt(b.cfg.ExecutablePath).exec(ctx, "session", "get", created.SessionID, "--json")
		get.Env = cmd.Env
		final, _ := get.Output()
		var row struct {
			State         string `json:"state"`
			ResultSummary string `json:"result_summary"`
		}
		_ = json.Unmarshal(final, &row)
		status := "failed"
		if row.State == "succeeded" {
			status = "completed"
		}
		result <- Result{Status: status, Output: row.ResultSummary, SessionID: created.SessionID}
	}()
	return &Session{Messages: messages, Result: result}, nil
}
