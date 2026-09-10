package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/agent"
)

const sessionsMaxSpendUSD = 3.5

var sessionsMCPScopes = []string{
	"mcp:databricks", "mcp:buildkite", "mcp:glean", "mcp:atlassian", "mcp:github", "mcp:github:write", "mcp:slack", "mcp:cloudzero", "mcp:datadog", "mcp:incident_io", "mcp:pagerduty", "mcp:sentry", "mcp:mixpanel_us", "mcp:mixpanel_eu", "mcp:launchdarkly", "mcp:granola", "mcp:bitrise",
}

var fullGitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// sessionsIntent is deliberately built once from the claim. It prevents a
// remote dispatch from observing mutable daemon state halfway through create.
type sessionsIntent struct {
	prompt, repository, thread string
	metadata, scopes           []string
	maxSpendUSD                float64
}

func buildSessionsIntent(task Task) (sessionsIntent, error) {
	if task.IssueID == "" || task.ChatSessionID != "" || task.AutopilotRunID != "" || task.QuickCreatePrompt != "" {
		return sessionsIntent{}, fmt.Errorf("Sessions runtime accepts issue tasks only")
	}
	if len(task.Repos) != 1 {
		return sessionsIntent{}, fmt.Errorf("Sessions runtime requires exactly one project GitHub repository")
	}
	repo, err := immutableGitHubRepository(task.Repos[0])
	if err != nil {
		return sessionsIntent{}, err
	}
	if task.Agent == nil {
		return sessionsIntent{}, fmt.Errorf("Sessions runtime requires claimed agent instructions")
	}
	return sessionsIntent{
		prompt:      buildSessionsPrompt(task, repo),
		repository:  repo,
		thread:      "multica:" + task.ID,
		metadata:    []string{"multica_task_id=" + task.ID, "multica_issue_id=" + task.IssueID},
		scopes:      append([]string(nil), sessionsMCPScopes...),
		maxSpendUSD: sessionsMaxSpendUSD,
	}, nil
}

func immutableGitHubRepository(repo RepoData) (string, error) {
	raw := strings.TrimSpace(repo.URL)
	ref := strings.TrimSpace(repo.Ref)
	if raw == "" || !fullGitSHA.MatchString(ref) {
		return "", fmt.Errorf("Sessions runtime requires a GitHub repository pinned to a full commit SHA")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("Sessions runtime requires an https github.com repository URL")
	}
	path := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("Sessions runtime requires owner/repository form")
	}
	return parts[0] + "/" + parts[1] + "@" + ref, nil
}

func buildSessionsPrompt(task Task, repository string) string {
	var b strings.Builder
	b.WriteString("You are handling one explicitly claimed Multica issue in a Runtime Cloud Session.\n\n")
	fmt.Fprintf(&b, "Issue: %s\n", task.IssueIdentifier)
	if task.ThreadName != "" {
		fmt.Fprintf(&b, "Title: %s\n", task.ThreadName)
	}
	if task.IssueDescription != "" {
		fmt.Fprintf(&b, "Description:\n%s\n", task.IssueDescription)
	}
	if task.Agent != nil && task.Agent.Instructions != "" {
		fmt.Fprintf(&b, "\nAgent instructions:\n%s\n", task.Agent.Instructions)
	}
	if task.WorkspaceContext != "" {
		fmt.Fprintf(&b, "\nWorkspace context:\n%s\n", task.WorkspaceContext)
	}
	if task.ProjectDescription != "" {
		fmt.Fprintf(&b, "\nProject context:\n%s\n", task.ProjectDescription)
	}
	if task.TriggerCommentContent != "" {
		fmt.Fprintf(&b, "\nCurrent trigger comment (%s):\n%s\n", task.TriggerAuthorName, task.TriggerCommentContent)
	}
	for _, comment := range task.CoalescedComments {
		fmt.Fprintf(&b, "\nCoalesced comment (%s):\n%s\n", comment.AuthorName, comment.Content)
	}
	fmt.Fprintf(&b, "\nRepository: %s\n", repository)
	b.WriteString("Work only on this issue. Do not call Multica. Return a concise final result summary when finished.\n")
	return b.String()
}

// runSessionsTask bypasses all local-agent preparation. Sessions has no local
// workdir, skills, MCP broker, token, resume, or daemon retry path.
func (d *Daemon) runSessionsTask(ctx context.Context, task Task, taskLog *slog.Logger) (TaskResult, error) {
	intent, err := buildSessionsIntent(task)
	if err != nil {
		return TaskResult{}, err
	}
	entry, ok := d.agents()["sessions"]
	if !ok || entry.Path == "" {
		return TaskResult{}, fmt.Errorf("Sessions runtime is unavailable")
	}
	if err := d.client.StartTask(ctx, task.ID); err != nil {
		return TaskResult{}, fmt.Errorf("start Sessions task: %w", err)
	}
	backend, err := agent.ResolveBackend("sessions", agent.Config{ExecutablePath: entry.Path, Logger: d.logger, TaskID: task.ID, RuntimeID: task.RuntimeID})
	if err != nil {
		return TaskResult{}, fmt.Errorf("create Sessions backend: %w", err)
	}
	var pinned bool
	session, err := backend.Execute(ctx, intent.prompt, agent.ExecOptions{Model: "devtools/standard", Sessions: &agent.SessionsExecOptions{
		Repository: intent.repository, Thread: intent.thread, Metadata: intent.metadata, MCPScopes: intent.scopes, MaxSpendUSD: intent.maxSpendUSD,
		PersistSessionID: func(_ context.Context, id string) error {
			pinCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := d.client.PinTaskSession(pinCtx, task.ID, id, ""); err != nil {
				return err
			}
			pinned = true
			return nil
		},
	}})
	if err != nil {
		return TaskResult{Status: "blocked", Comment: err.Error(), FailureReason: "dispatch_unknown"}, nil
	}
	d.runningTasks.Add(1)
	defer d.runningTasks.Add(-1)
	// Drain before accepting the result so the timeline has received the watcher
	// tail. This path intentionally has no local watchdog or retry.
	d.sessionsTaskMessages(ctx, task.ID, session.Messages)
	result := <-session.Result
	if result.Status == "completed" {
		return TaskResult{Status: "completed", Comment: result.Output, SessionID: result.SessionID}, nil
	}
	if result.Status == "cancelled" {
		return TaskResult{Status: "blocked", Comment: "Sessions run cancelled", SessionID: result.SessionID, FailureReason: "dispatch_unknown", RemoteCleanupStatus: "confirmed"}, nil
	}
	if !pinned && result.SessionID != "" {
		return TaskResult{Status: "blocked", Comment: "Sessions dispatch outcome could not be pinned", SessionID: result.SessionID, FailureReason: "dispatch_unknown"}, nil
	}
	return TaskResult{Status: "blocked", Comment: result.Error, SessionID: result.SessionID, FailureReason: "dispatch_unknown"}, nil
}

// sessionsTaskMessages projects only approved watcher fields into the existing
// task timeline. Completion is always decided by the final Session GET.
func (d *Daemon) sessionsTaskMessages(ctx context.Context, taskID string, messages <-chan agent.Message) {
	seq := 0
	for message := range messages {
		var row TaskMessageData
		switch message.Type {
		case agent.MessageText:
			row = TaskMessageData{Type: "text", Content: message.Content}
		case agent.MessageToolUse:
			row = TaskMessageData{Type: "tool_use", Tool: message.Tool, Content: message.Content}
		case agent.MessageToolResult:
			row = TaskMessageData{Type: "tool_result", Tool: message.Tool}
		default:
			continue
		}
		seq++
		row.Seq = seq
		if err := d.client.ReportTaskMessages(ctx, taskID, []TaskMessageData{row}); err != nil {
			d.logger.Debug("failed to report Sessions task message", "task_id", taskID, "error", err)
		}
	}
}
