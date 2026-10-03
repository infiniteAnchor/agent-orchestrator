package taskauto

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestFailedResultRetriesOnceAndReviewsOnce(t *testing.T) {
	svc, db, project := newAuto(t)
	if err := db.EnsurePlannerCursor(context.Background(), fixedTime().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	planID, attemptID := failTask(t, db, project, domain.TaskResultFailed)
	reviewer := &countingReviewer{summary: "handoff for the next worker"}
	svc.reviewer = reviewer
	if err := svc.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reviewer.n != 1 {
		t.Fatalf("reviewer calls = %d, want 1", reviewer.n)
	}
	view := mustSchedule(t, db, project, planID)
	node := taskNode(t, view, "build")
	if node.State != domain.TaskStateQueued || node.WorkspaceKey != "ws-build" || node.Harness != string(domain.HarnessCodex) {
		t.Fatalf("task after retry = %+v", node)
	}
	if len(view.Attempts) != 1 || view.Attempts[0].ID != attemptID || view.Attempts[0].State != domain.TaskAttemptStateFailed {
		t.Fatalf("attempts = %+v, want the original failed attempt kept", view.Attempts)
	}
	handoffs, err := svc.ListHandoffs(context.Background(), domain.ProjectID(project), planID, "build")
	if err != nil || len(handoffs) != 1 || handoffs[0].Summary != reviewer.summary {
		t.Fatalf("handoffs = %+v err %v", handoffs, err)
	}
}

func TestCursorIgnoresResultsAlreadyInTheLog(t *testing.T) {
	svc, db, project := newAuto(t)
	planID, _ := failTask(t, db, project, domain.TaskResultFailed)
	if err := svc.store.EnsurePlannerCursor(context.Background(), fixedTime()); err != nil {
		t.Fatal(err)
	}
	reviewer := &countingReviewer{summary: "should not run"}
	svc.reviewer = reviewer
	if err := svc.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reviewer.n != 0 {
		t.Fatalf("historical result started %d reviews", reviewer.n)
	}
	node := taskNode(t, mustSchedule(t, db, project, planID), "build")
	if node.State != domain.TaskStateFailed {
		t.Fatalf("state = %s, want failed", node.State)
	}
}

func TestInconclusiveResultIsNotRetried(t *testing.T) {
	svc, db, project := newAuto(t)
	if err := db.EnsurePlannerCursor(context.Background(), fixedTime().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	planID, attemptID := failTask(t, db, project, domain.TaskResultInconclusive)
	if err := svc.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	node := taskNode(t, mustSchedule(t, db, project, planID), "build")
	if node.State != domain.TaskStateBlocked {
		t.Fatalf("state = %s, want blocked", node.State)
	}
	outcome, err := svc.Retry(context.Background(), domain.ProjectID(project), planID, "build", RetryInput{
		AttemptID: attemptID, Reason: domain.RetryReasonFailed,
	})
	if err != nil || outcome.Decision.Action != domain.RetryActionHold || !outcome.Repeated {
		t.Fatalf("retry = %+v err %v, want the original hold", outcome, err)
	}
	node = taskNode(t, mustSchedule(t, db, project, planID), "build")
	if node.State != domain.TaskStateBlocked {
		t.Fatalf("state after refused retry = %s", node.State)
	}
}

func TestExplicitRetryOfBlockedAttemptIsRefused(t *testing.T) {
	svc, db, project := newAuto(t)
	planID, attemptID := failTask(t, db, project, domain.TaskResultInconclusive)
	_, err := svc.Retry(context.Background(), domain.ProjectID(project), planID, "build", RetryInput{
		AttemptID: attemptID, Reason: domain.RetryReasonFailed,
	})
	var api *apierr.Error
	if err == nil || !errors.As(err, &api) || api.Code != "TASK_RETRY_AMBIGUOUS" {
		t.Fatalf("retry error = %v, want TASK_RETRY_AMBIGUOUS", err)
	}
	node := taskNode(t, mustSchedule(t, db, project, planID), "build")
	if node.State != domain.TaskStateBlocked {
		t.Fatalf("state = %s, want blocked", node.State)
	}
}

func TestProviderExhaustionFallsBackWithoutChangingWorkspace(t *testing.T) {
	svc, db, project := newAuto(t)
	if _, err := svc.PutPolicy(context.Background(), domain.ProjectID(project), domain.TaskRetryPolicy{
		MaxAttempts: 2, FallbackHarness: string(domain.HarnessOpenCode),
	}); err != nil {
		t.Fatal(err)
	}
	planID, attemptID := failTask(t, db, project, domain.TaskResultFailed)
	first, err := svc.Retry(context.Background(), domain.ProjectID(project), planID, "build", RetryInput{
		AttemptID: attemptID, Reason: domain.RetryReasonProviderExhausted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision.Action != domain.RetryActionFallback || first.Decision.Harness != string(domain.HarnessOpenCode) {
		t.Fatalf("decision = %+v", first.Decision)
	}
	again, err := svc.Retry(context.Background(), domain.ProjectID(project), planID, "build", RetryInput{
		AttemptID: attemptID, Reason: domain.RetryReasonProviderExhausted,
	})
	if err != nil || again.Decision.ID != first.Decision.ID || !again.Repeated {
		t.Fatalf("repeat = %+v err %v", again, err)
	}
	node := taskNode(t, mustSchedule(t, db, project, planID), "build")
	if node.State != domain.TaskStateQueued || node.Harness != string(domain.HarnessOpenCode) || node.WorkspaceKey != "ws-build" {
		t.Fatalf("task = %+v", node)
	}
}

func TestAttemptCapEscalatesToAHumanGate(t *testing.T) {
	svc, db, project := newAuto(t)
	if _, err := svc.PutPolicy(context.Background(), domain.ProjectID(project), domain.TaskRetryPolicy{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsurePlannerCursor(context.Background(), fixedTime().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	planID, _ := failTask(t, db, project, domain.TaskResultFailed)
	if err := svc.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	node := taskNode(t, mustSchedule(t, db, project, planID), "build")
	if node.State != domain.TaskStateFailed {
		t.Fatalf("state = %s, want failed", node.State)
	}
	gates, err := svc.ListGates(context.Background(), domain.ProjectID(project), planID)
	if err != nil || len(gates) != 1 || gates[0].State != domain.HumanGatePending {
		t.Fatalf("gates = %+v err %v", gates, err)
	}
	if err := svc.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	gates, err = svc.ListGates(context.Background(), domain.ProjectID(project), planID)
	if err != nil || len(gates) != 1 {
		t.Fatalf("second consume gates = %+v err %v", gates, err)
	}
}

func TestHumanGateBlocksClaimUntilApproved(t *testing.T) {
	svc, db, project := newAuto(t)
	planID := createPlan(t, db, project)
	gate, err := svc.OpenGate(context.Background(), domain.ProjectID(project), planID, "build", GateInput{
		RequestKey: "review-build", Summary: "Need a person before this starts",
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.OpenGate(context.Background(), domain.ProjectID(project), planID, "build", GateInput{
		RequestKey: "review-build", Summary: "Need a person before this starts",
	})
	if err != nil || again.ID != gate.ID {
		t.Fatalf("repeat gate = %+v err %v", again, err)
	}
	claim, err := db.ClaimReadyTask(context.Background(), domain.ProjectID(project), planID, "build", "attempt-blocked", domain.ScheduleLimits{}, fixedTime())
	if err != nil || claim.Reason != domain.ClaimHumanGate {
		t.Fatalf("claim = %+v err %v", claim, err)
	}
	if _, err := svc.ResolveGate(context.Background(), domain.ProjectID(project), planID, gate.ID, domain.HumanGateApproved); err != nil {
		t.Fatal(err)
	}
	claim, err = db.ClaimReadyTask(context.Background(), domain.ProjectID(project), planID, "build", "attempt-ok", domain.ScheduleLimits{}, fixedTime())
	if err != nil || claim.Reason != "" || claim.Attempt.ID != "attempt-ok" {
		t.Fatalf("claim after approval = %+v err %v", claim, err)
	}
}

func TestRejectedGateCancelsQueuedWork(t *testing.T) {
	svc, db, project := newAuto(t)
	planID := createPlan(t, db, project)
	gate, err := svc.OpenGate(context.Background(), domain.ProjectID(project), planID, "build", GateInput{
		RequestKey: "stop-build", Summary: "Do not run this",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := svc.ResolveGate(context.Background(), domain.ProjectID(project), planID, gate.ID, domain.HumanGateRejected)
	if err != nil || resolved.State != domain.HumanGateRejected {
		t.Fatalf("resolved = %+v err %v", resolved, err)
	}
	node := taskNode(t, mustSchedule(t, db, project, planID), "build")
	if node.State != domain.TaskStateCancelled {
		t.Fatalf("state = %s, want cancelled", node.State)
	}
	if _, err := svc.ResolveGate(context.Background(), domain.ProjectID(project), planID, gate.ID, domain.HumanGateRejected); err != nil {
		t.Fatal(err)
	}
}

type countingReviewer struct {
	n       int
	summary string
}

func (c *countingReviewer) Review(context.Context, domain.PlannerFollowup) (string, error) {
	c.n++
	return c.summary, nil
}

func newAuto(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	db := sqlitetest.MustOpen(t)
	project := "proj"
	if err := db.UpsertProject(context.Background(), domain.ProjectRecord{
		ID: project, Path: t.TempDir(), RegisteredAt: fixedTime(),
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(db, nil)
	svc.syncReview = true
	svc.now = fixedTime
	var n int
	svc.newID = func() string {
		n++
		return "auto-" + string(rune('a'+n-1))
	}
	return svc, db, project
}

func fixedTime() time.Time { return time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC) }

var planSeq int

func createPlan(t *testing.T, db *store.Store, project string) string {
	t.Helper()
	planSeq++
	plan := domain.TaskPlan{
		ID: "plan-" + strconv.Itoa(planSeq), ProjectID: project, Title: "Ship",
		Tasks: []domain.PlannedTask{{
			ID: "build", Title: "Build", Prompt: "do the work", WorkspaceKey: "ws-build",
			Harness: string(domain.HarnessCodex), VerificationCommands: []string{"true"},
		}},
	}
	if _, err := db.CreateTaskPlan(context.Background(), plan, fixedTime()); err != nil {
		t.Fatal(err)
	}
	return plan.ID
}

func failTask(t *testing.T, db *store.Store, project string, outcome domain.TaskResultOutcome) (string, string) {
	t.Helper()
	planID := createPlan(t, db, project)
	claim, err := db.ClaimReadyTask(context.Background(), domain.ProjectID(project), planID, "build", "attempt-"+planID, domain.ScheduleLimits{}, fixedTime())
	if err != nil || claim.Reason != "" {
		t.Fatalf("claim = %+v err %v", claim, err)
	}
	ok, err := db.CommitTaskResult(context.Background(), domain.TaskResult{
		ID: "result-" + planID, PlanID: planID, TaskID: "build", AttemptID: claim.Attempt.ID,
		Outcome: outcome, Summary: "worker stopped", Evidence: []byte(`{"passed":false}`), RecordedAt: fixedTime(),
	})
	if err != nil || !ok {
		t.Fatalf("commit result ok=%v err=%v", ok, err)
	}
	return planID, claim.Attempt.ID
}

func mustSchedule(t *testing.T, db *store.Store, project, planID string) domain.TaskScheduleView {
	t.Helper()
	view, ok, err := db.LoadTaskSchedule(context.Background(), domain.ProjectID(project), planID)
	if err != nil || !ok {
		t.Fatalf("schedule ok=%v err=%v", ok, err)
	}
	return view
}

func taskNode(t *testing.T, view domain.TaskScheduleView, id string) domain.ScheduleNode {
	t.Helper()
	for _, node := range view.Nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("task %s missing", id)
	return domain.ScheduleNode{}
}
