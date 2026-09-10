package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/pkg/agent"
)

func TestBuildSessionsIntentIsPinnedOrSupportedBranchAndIssueOnly(t *testing.T) {
	task := Task{ID: "task-1", IssueID: "issue-1", IssueIdentifier: "MUL-1", ThreadName: "Fix it", IssueDescription: "The thing is broken.", WorkspaceContext: "be careful", ProjectDescription: "project", TriggerCommentContent: "please fix", Agent: &AgentData{Instructions: "write tests"}, Repos: []RepoData{{URL: "https://github.com/acme/widget.git", Ref: "0123456789abcdef0123456789abcdef01234567"}}}
	intent, err := buildSessionsIntent(task)
	if err != nil {
		t.Fatal(err)
	}
	if intent.repository != "acme/widget@0123456789abcdef0123456789abcdef01234567" || intent.thread != "multica:task-1" {
		t.Fatalf("intent = %+v", intent)
	}
	if !reflect.DeepEqual(intent.scopes, sessionsMCPScopes) {
		t.Fatalf("scopes = %v", intent.scopes)
	}
	for _, required := range []string{"Fix it", "The thing is broken.", "write tests", "be careful", "project", "please fix", intent.repository} {
		if !strings.Contains(intent.prompt, required) {
			t.Errorf("prompt missing %q", required)
		}
	}
	if strings.Contains(intent.prompt, "multica issue") {
		t.Fatal("remote prompt must not require a Multica credential")
	}
	task.Repos[0].Ref = strings.Repeat("f", 40)
	if intent.repository != "acme/widget@0123456789abcdef0123456789abcdef01234567" {
		t.Fatal("intent changed after claim mutation")
	}
}

func TestBuildSessionsIntentAllowsSupportedBranch(t *testing.T) {
	task := Task{ID: "task-branch", IssueID: "issue-branch", Agent: &AgentData{}, Repos: []RepoData{{URL: "https://github.com/samsara-dev/devbox-client", Ref: "main"}}}
	intent, err := buildSessionsIntent(task)
	if err != nil {
		t.Fatalf("buildSessionsIntent(main): %v", err)
	}
	if intent.repository != "samsara-dev/devbox-client@main" {
		t.Fatalf("repository = %q", intent.repository)
	}
}

func TestBuildSessionsIntentRejectsUnsafeTargets(t *testing.T) {
	base := Task{ID: "t", IssueID: "i", Agent: &AgentData{}, Repos: []RepoData{{URL: "https://github.com/acme/widget", Ref: strings.Repeat("a", 40)}}}
	cases := []Task{func() Task { v := base; v.ChatSessionID = "chat"; return v }(), func() Task { v := base; v.Repos = nil; return v }(), func() Task {
		v := base
		v.Repos = []RepoData{{URL: "git@github.com:acme/widget.git", Ref: strings.Repeat("a", 40)}}
		return v
	}(), func() Task {
		v := base
		v.Repos = []RepoData{{URL: "https://github.com/acme/widget", Ref: "feature bad ref"}}
		return v
	}()}
	for _, task := range cases {
		if _, err := buildSessionsIntent(task); err == nil {
			t.Fatalf("unsafe task %+v was accepted", task)
		}
	}
}

func TestSessionsTaskResultConfirmsOnlyTerminalCancellation(t *testing.T) {
	cases := []struct {
		name        string
		result      agent.Result
		pinned      bool
		wantCleanup string
	}{
		{name: "terminal cancelled readback", result: agent.Result{Status: "cancelled", SessionID: "sess-1"}, pinned: true, wantCleanup: "confirmed"},
		{name: "final read failed", result: agent.Result{Status: "failed", Error: "Sessions final read failed", SessionID: "sess-1"}, pinned: true},
		{name: "nonterminal final read", result: agent.Result{Status: "failed", Error: "Sessions final state is nonterminal: running", SessionID: "sess-1"}, pinned: true},
		{name: "un-pinned dispatch", result: agent.Result{Status: "failed", SessionID: "sess-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionsTaskResult(tc.result, tc.pinned)
			if got.RemoteCleanupStatus != tc.wantCleanup {
				t.Fatalf("RemoteCleanupStatus = %q, want %q (result: %+v)", got.RemoteCleanupStatus, tc.wantCleanup, got)
			}
		})
	}
}
