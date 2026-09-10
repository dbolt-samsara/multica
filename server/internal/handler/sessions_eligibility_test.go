package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

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
