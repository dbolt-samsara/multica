package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestSessionsCommentDelegationStoresPerTaskModelOverride(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createProviderRuntime(t, "sessions")
	agentID := dbfx.Agent(t, "sessions model override", runtimeID, testutil.Cols{
		"owner_id":        testUserID,
		"permission_mode": "public_to",
	})
	issueID := dbfx.Issue(t, "Sessions per-task model", testutil.Cols{
		"creator_type": "member", "creator_id": testUserID,
	})
	controllerRuntimeID := handlerTestRuntimeID(t)
	controllerID := dbfx.Agent(t, "sessions model controller", controllerRuntimeID, testutil.Cols{
		"owner_id":        testUserID,
		"permission_mode": "private",
	})
	controllerTaskID := dbfx.Task(t, controllerID, testutil.Cols{
		"runtime_id":          controllerRuntimeID,
		"issue_id":            issueID,
		"status":              "running",
		"started_at":          testutil.Raw("now()"),
		"originator_user_id":  testUserID,
		"accountable_user_id": testUserID,
	})
	content := fmt.Sprintf("[@Sessions Worker](mention://agent/%s) research this", agentID)
	preview := testutil.Call(t, testHandler.PreviewCommentTriggers, testutil.WithURLParams(
		asRun(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments/trigger-preview", map[string]any{
			"content": content,
		}), controllerID, controllerTaskID), "id", issueID,
	)).Want(http.StatusOK)
	var previewBody CommentTriggerPreviewResponse
	preview.JSON(&previewBody)
	if len(previewBody.Agents) != 1 || previewBody.Agents[0].RuntimeID != runtimeID || previewBody.Agents[0].RuntimeProvider != "sessions" || previewBody.Agents[0].RuntimeOnline == nil {
		t.Fatalf("preview agents = %+v, want Sessions runtime metadata", previewBody.Agents)
	}
	resp := testutil.Call(t, testHandler.CreateComment, testutil.WithURLParams(
		asRun(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
			"content": content,
			"model":   "  claude-fable-5-1  ",
		}), controllerID, controllerTaskID), "id", issueID,
	)).Want(http.StatusCreated)
	var comment CommentResponse
	resp.JSON(&comment)
	if len(comment.TriggerOutcomes) != 1 || comment.TriggerOutcomes[0].Status != DispatchQueued {
		t.Fatalf("trigger outcomes = %+v, want one queued run", comment.TriggerOutcomes)
	}
	tasks, err := testHandler.Queries.ListTasksByIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatalf("list issue tasks: %v", err)
	}
	var delegatedTask *db.AgentTaskQueue
	for i := range tasks {
		if tasks[i].AgentID == parseUUID(agentID) {
			delegatedTask = &tasks[i]
			break
		}
	}
	if delegatedTask == nil || !delegatedTask.ModelOverride.Valid || delegatedTask.ModelOverride.String != "claude-fable-5-1" {
		t.Fatalf("tasks = %+v, want delegated task with trimmed model override", tasks)
	}
	wire := taskToResponse(*delegatedTask, testWorkspaceID)
	if wire.ModelOverride != "claude-fable-5-1" {
		t.Fatalf("claim model_override = %q", wire.ModelOverride)
	}

	second := testutil.Call(t, testHandler.CreateComment, testutil.WithURLParams(
		asRun(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
			"content":   content,
			"parent_id": comment.ID,
			"model":     "devtools/standard",
		}), controllerID, controllerTaskID), "id", issueID,
	)).Want(http.StatusCreated)
	var secondComment CommentResponse
	second.JSON(&secondComment)
	if len(secondComment.TriggerOutcomes) != 1 || secondComment.TriggerOutcomes[0].Status != DispatchBlocked || secondComment.TriggerOutcomes[0].ReasonCode != ReasonAlreadyActive {
		t.Fatalf("second trigger outcomes = %+v, want blocked/already_active", secondComment.TriggerOutcomes)
	}
	tasks, err = testHandler.Queries.ListTasksByIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatalf("list issue tasks after blocked override: %v", err)
	}
	delegatedCount := 0
	for _, task := range tasks {
		if task.AgentID == parseUUID(agentID) {
			delegatedCount++
			if task.ModelOverride.String != "claude-fable-5-1" {
				t.Fatalf("blocked override changed delegated task: %+v", task)
			}
		}
	}
	if delegatedCount != 1 {
		t.Fatalf("blocked override changed active task: %+v", tasks)
	}
}

func TestCommentModelOverrideRejectsUnsupportedRuntimeBeforeSaving(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := createProviderRuntime(t, "mcode")
	agentID := dbfx.Agent(t, "unsupported model target", runtimeID, testutil.Cols{"owner_id": testUserID})
	issueID := dbfx.Issue(t, "Reject unsupported model", testutil.Cols{
		"creator_type": "member", "creator_id": testUserID,
	})
	content := fmt.Sprintf("[@Local Worker](mention://agent/%s) research this", agentID)
	testutil.Call(t, testHandler.CreateComment, testutil.WithURLParams(
		newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{
			"content": content,
			"model":   "claude-fable-5-1",
		}), "id", issueID,
	)).Want(http.StatusBadRequest)
}

func TestCreateIssueStoresModelOverrideOnAutomaticTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, provider := range []string{"sessions", "codex"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			runtimeID := createProviderRuntime(t, provider)
			agentID := dbfx.Agent(t, provider+" create model override", runtimeID, testutil.Cols{
				"owner_id":        testUserID,
				"permission_mode": "private",
			})
			resp := testutil.Call(t, testHandler.CreateIssue,
				newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
					"title":         provider + " create model override",
					"status":        "todo",
					"assignee_type": "agent",
					"assignee_id":   agentID,
					"model":         "  selected/model  ",
				}),
			).Want(http.StatusCreated)
			var issue IssueResponse
			resp.JSON(&issue)
			t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue WHERE id = $1`, issue.ID) })

			tasks, err := testHandler.Queries.ListTasksByIssue(ctx, parseUUID(issue.ID))
			if err != nil {
				t.Fatalf("list created issue tasks: %v", err)
			}
			if len(tasks) != 1 || !tasks[0].ModelOverride.Valid || tasks[0].ModelOverride.String != "selected/model" {
				t.Fatalf("tasks = %+v, want one automatic task with model override", tasks)
			}
		})
	}
}

func TestCreateIssueRejectsModelOverrideForUnsupportedRuntime(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID := createProviderRuntime(t, "mcode")
	agentID := dbfx.Agent(t, "unsupported create model target", runtimeID, testutil.Cols{"owner_id": testUserID})
	testutil.Call(t, testHandler.CreateIssue,
		newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
			"title":         "Reject unsupported create model",
			"status":        "todo",
			"assignee_type": "agent",
			"assignee_id":   agentID,
			"model":         "claude-fable-5-1",
		}),
	).Want(http.StatusBadRequest)
}

// TestSessionsEligibilityGates proves that a Sessions-bound agent cannot be
// selected by Chat surfaces or by quick-create, and that the only HTTP issue
// admission is its private owner's direct assignment.
func TestSessionsEligibilityGates(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID := createProviderRuntime(t, "sessions")
	agentID := dbfx.Agent(t, "sessions eligibility", runtimeID, testutil.Cols{
		"owner_id":        testUserID,
		"permission_mode": "private",
	})
	agentUUID := parseUUID(agentID)

	// The generic agent list feeds issue assignment as well as management UI.
	// Sessions must remain selectable for its one permitted path; only the
	// dedicated Chat surfaces may suppress it.
	listed := testutil.Call(t, testHandler.ListAgents,
		newRequest(http.MethodGet, "/api/agents?workspace_id="+testWorkspaceID, nil),
	).Want(http.StatusOK)
	if !listContainsAgent(t, listed.Body.Bytes(), agentID) {
		t.Fatalf("owner ListAgents omitted Sessions issue worker %s", agentID)
	}

	ownerReq := newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, nil)
	if status, message := testHandler.validateAssigneePair(ctx, ownerReq, testWorkspaceID, pgtype.Text{String: "agent", Valid: true}, agentUUID); status != 0 {
		t.Fatalf("owner direct issue admission = (%d, %q), want success", status, message)
	}
	otherReq := newRequestAs("00000000-0000-0000-0000-000000000123", http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, nil)
	if status, _ := testHandler.validateAssigneePair(ctx, otherReq, testWorkspaceID, pgtype.Text{String: "agent", Valid: true}, agentUUID); status != http.StatusForbidden {
		t.Fatalf("non-owner Sessions issue admission = %d, want %d", status, http.StatusForbidden)
	}

	testutil.Call(t, testHandler.QuickCreateIssue, newRequest(http.MethodPost, "/api/issues/quick-create?workspace_id="+testWorkspaceID, map[string]any{
		"agent_id": agentID, "prompt": "create an issue",
	})).Want(http.StatusBadRequest)
	if _, err := testHandler.TaskService.EnqueueQuickCreateTask(ctx, parseUUID(testWorkspaceID), parseUUID(testUserID), agentUUID, pgtype.UUID{}, "create an issue", "", "", pgtype.UUID{}, pgtype.UUID{}, nil); !errors.Is(err, service.ErrSessionsInvocationNotAllowed) {
		t.Fatalf("service Sessions quick-create error = %v, want ErrSessionsInvocationNotAllowed", err)
	}

	issueID := dbfx.Issue(t, "Sessions owner claim", testutil.Cols{
		"assignee_type": "agent", "assignee_id": agentID,
		"creator_type": "member", "creator_id": testUserID,
	})
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatalf("load claim issue: %v", err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("owner direct issue claim: %v", err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssueWithHandoff(ctx, issue, "handoff", parseUUID(testUserID)); !errors.Is(err, service.ErrSessionsInvocationNotAllowed) {
		t.Fatalf("Sessions handoff error = %v, want ErrSessionsInvocationNotAllowed", err)
	}

	// A Sessions worker may opt in to controller delegation by using public_to.
	// The delegator must be a live task of another agent owned by the same human,
	// and the queued child must preserve that task as provenance.
	delegatedWorkerID := dbfx.Agent(t, "sessions delegated worker", runtimeID, testutil.Cols{
		"owner_id":        testUserID,
		"permission_mode": "public_to",
	})
	controllerID := dbfx.Agent(t, "sessions same-owner controller", handlerTestRuntimeID(t), testutil.Cols{
		"owner_id":        testUserID,
		"permission_mode": "private",
	})
	controllerTaskID := dbfx.Task(t, controllerID, testutil.Cols{
		"runtime_id":          handlerTestRuntimeID(t),
		"status":              "running",
		"started_at":          testutil.Raw("now()"),
		"originator_user_id":  testUserID,
		"accountable_user_id": testUserID,
	})
	delegatedReq := asRun(newRequest(http.MethodPost, "/api/issues?workspace_id="+testWorkspaceID, nil), controllerID, controllerTaskID)
	if status, _ := testHandler.validateAssigneePair(ctx, delegatedReq, testWorkspaceID, pgtype.Text{String: "agent", Valid: true}, agentUUID); status != http.StatusForbidden {
		t.Fatalf("private Sessions worker delegated admission = %d, want %d", status, http.StatusForbidden)
	}
	if status, message := testHandler.validateAssigneePair(ctx, delegatedReq, testWorkspaceID, pgtype.Text{String: "agent", Valid: true}, parseUUID(delegatedWorkerID)); status != 0 {
		t.Fatalf("same-owner controller issue admission = (%d, %q), want success", status, message)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'queued' WHERE id = $1`, controllerTaskID)
	if status, _ := testHandler.validateAssigneePair(ctx, delegatedReq, testWorkspaceID, pgtype.Text{String: "agent", Valid: true}, parseUUID(delegatedWorkerID)); status != http.StatusForbidden {
		t.Fatalf("non-running controller issue admission = %d, want %d", status, http.StatusForbidden)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, controllerTaskID)
	delegatedIssueID := dbfx.Issue(t, "Sessions delegated issue", testutil.Cols{
		"assignee_type": "agent", "assignee_id": delegatedWorkerID,
		"creator_type": "agent", "creator_id": controllerID,
		"origin_type": "agent_create", "origin_id": controllerTaskID,
	})
	delegatedIssue, err := testHandler.Queries.GetIssue(ctx, parseUUID(delegatedIssueID))
	if err != nil {
		t.Fatalf("load delegated issue: %v", err)
	}
	delegatedTask, err := testHandler.TaskService.EnqueueTaskForIssueDelegated(ctx, delegatedIssue, parseUUID(controllerTaskID))
	if err != nil {
		t.Fatalf("same-owner delegated Sessions task: %v", err)
	}
	if delegatedTask.DelegatedFromTaskID != parseUUID(controllerTaskID) {
		t.Fatalf("delegated_from_task_id = %s, want %s", delegatedTask.DelegatedFromTaskID.Bytes, controllerTaskID)
	}

	testutil.Call(t, testHandler.PinChatAgent,
		withChatTestWorkspaceCtx(t, newRequest(http.MethodPost, "/api/chat/pinned-agents", map[string]any{"agent_id": agentID})),
	).Want(http.StatusBadRequest)

	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_pinned_agent (workspace_id, user_id, agent_id, position)
		VALUES ($1, $2, $3, 1)
	`, testWorkspaceID, testUserID, agentID); err != nil {
		t.Fatalf("seed Sessions Chat pin: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM chat_pinned_agent WHERE workspace_id = $1 AND user_id = $2 AND agent_id = $3`, testWorkspaceID, testUserID, agentID)
	})
	list := testutil.Call(t, testHandler.ListChatPinnedAgents,
		withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/chat/pinned-agents", nil)),
	).Want(http.StatusOK)
	var pinned []ChatPinnedAgentResponse
	list.JSON(&pinned)
	if len(pinned) != 0 {
		t.Fatalf("Sessions agent leaked into Chat pin list: %#v", pinned)
	}
}
