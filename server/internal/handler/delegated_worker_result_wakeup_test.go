package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestCompleteTask_DelegatedWorkerResultResumesSquadLeader reproduces DEV-36:
// a runtime-cloud worker returns terminal text, TaskService synthesizes the
// reply inside the delegation thread, and no Handler.CreateComment trigger runs.
// Complete, retryable/stopped, and genuine HumanGate results must all wake the
// coordinator for evaluation, while their contents remain untouched so the
// leader—not routing code—preserves the gate. Repeated outcomes coalesce onto
// one leader task, proving the one-writer/idempotency boundary.
func TestCompleteTask_DelegatedWorkerResultResumesSquadLeader(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
	})

	var leaderRuntimeID, workerRuntimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, fx.LeaderID).Scan(&leaderRuntimeID)
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, fx.OtherID).Scan(&workerRuntimeID)
	leaderTaskID := dbfx.Task(t, fx.LeaderID, testutil.Cols{
		"runtime_id":          leaderRuntimeID,
		"issue_id":            issueID,
		"status":              "completed",
		"is_leader_task":      true,
		"squad_id":            fx.SquadID,
		"originator_user_id":  testUserID,
		"accountable_user_id": testUserID,
		"started_at":          testutil.Raw("now() - interval '10 minutes'"),
		"completed_at":        testutil.Raw("now() - interval '9 minutes'"),
	})
	delegationRootID := dbfx.Insert(t, "comment", testutil.Cols{
		"issue_id":       issueID,
		"workspace_id":   testWorkspaceID,
		"author_type":    "agent",
		"author_id":      fx.LeaderID,
		"content":        "[@Worker](mention://agent/" + fx.OtherID + ") inspect and report",
		"type":           "comment",
		"source_task_id": leaderTaskID,
	})

	outcomes := []string{
		"complete — implementation and tests passed",
		"stopped — exact checkout is unavailable; retry from the required ref",
		"blocked — a HumanGate is required before merge",
	}
	workerTasks := make([]string, 0, len(outcomes))
	for i, output := range outcomes {
		workerTaskID := dbfx.Task(t, fx.OtherID, testutil.Cols{
			"runtime_id":             workerRuntimeID,
			"issue_id":               issueID,
			"status":                 "running",
			"trigger_comment_id":     delegationRootID,
			"comment_thread_id":      delegationRootID,
			"delivered_comment_ids":  testutil.Raw("ARRAY['" + delegationRootID + "'::uuid]"),
			"delegated_from_task_id": leaderTaskID,
			"originator_user_id":     testUserID,
			"accountable_user_id":    testUserID,
			"started_at":             testutil.Raw("now()"),
		})
		workerTasks = append(workerTasks, workerTaskID)
		if w := completeTaskViaHandler(t, workerTaskID, output); w.Code != 200 {
			t.Fatalf("outcome %d CompleteTask: got %d: %s", i, w.Code, w.Body.String())
		}
	}

	var queued int
	dbfx.QueryRow(t, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'
		  AND is_leader_task AND squad_id = $3
	`, issueID, fx.LeaderID, fx.SquadID).Scan(&queued)
	if queued != 1 {
		t.Fatalf("delegated worker outcomes queued %d leader tasks, want exactly 1", queued)
	}

	for i, workerTaskID := range workerTasks {
		var content string
		var parentID string
		var covered bool
		dbfx.QueryRow(t, `
			SELECT c.content, c.parent_id, EXISTS (
				SELECT 1 FROM agent_task_queue leader
				WHERE leader.issue_id = c.issue_id AND leader.agent_id = $2
				  AND leader.status = 'queued' AND leader.is_leader_task
				  AND (leader.trigger_comment_id = c.id OR c.id = ANY(leader.coalesced_comment_ids))
			)
			FROM comment c
			WHERE c.source_task_id = $1 AND c.author_type = 'agent' AND c.type = 'comment'
			ORDER BY c.created_at DESC, c.id DESC LIMIT 1
		`, workerTaskID, fx.LeaderID).Scan(&content, &parentID, &covered)
		if content != outcomes[i] {
			t.Errorf("worker result %d content = %q, want unchanged %q", i, content, outcomes[i])
		}
		if parentID != delegationRootID {
			t.Errorf("worker result %d parent = %q, want delegation root %q", i, parentID, delegationRootID)
		}
		if !covered {
			t.Errorf("worker result %d was not delivered to the leader continuation", i)
		}
	}

	// A repeated terminal callback is idempotent and cannot add a second writer.
	if w := completeTaskViaHandler(t, workerTasks[0], outcomes[0]); w.Code != 200 {
		t.Fatalf("repeated CompleteTask: got %d: %s", w.Code, w.Body.String())
	}
	dbfx.QueryRow(t, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND is_leader_task
	`, issueID, fx.LeaderID).Scan(&queued)
	if queued != 1 {
		t.Fatalf("repeated completion queued %d leader tasks, want exactly 1", queued)
	}
}

// An arbitrary agent task on a squad-owned issue is not delegation proof. Its
// synthesized output must not acquire coordinator privileges or start a loop.
func TestCompleteTask_OrdinaryAgentOutputDoesNotWakeSquadLeader(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, issueID)
	})
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, fx.OtherID).Scan(&runtimeID)
	taskID := dbfx.Task(t, fx.OtherID, testutil.Cols{
		"runtime_id": runtimeID,
		"issue_id":   issueID,
		"status":     "running",
		"started_at": testutil.Raw("now()"),
	})
	if w := completeTaskViaHandler(t, taskID, "ordinary agent output"); w.Code != 200 {
		t.Fatalf("CompleteTask: got %d: %s", w.Code, w.Body.String())
	}
	var queued int
	dbfx.QueryRow(t, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND is_leader_task
	`, issueID, fx.LeaderID).Scan(&queued)
	if queued != 0 {
		t.Fatalf("ordinary agent output queued %d leader tasks, want 0", queued)
	}
}
