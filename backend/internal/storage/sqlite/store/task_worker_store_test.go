package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestTaskWorkerSessionAtomicBinding(t *testing.T) {
	for _, tc := range []string{"success", "unleased", "project mismatch", "kind mismatch", "harness mismatch", "insert failure"} {
		t.Run(tc, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			seedProject(t, s, "mer")
			seedProject(t, s, "other")
			now := time.Now().UTC()
			_, err := s.CreateTaskPlan(ctx, domain.TaskPlan{ID: "plan", ProjectID: "mer", Title: "plan", Phases: []domain.TaskPhase{{ID: "phase", Title: "phase"}}, Tasks: []domain.PlannedTask{{ID: "task", PhaseID: "phase", Title: "task", Prompt: "work", Harness: string(domain.HarnessClaudeCode), VerificationCommands: []string{"true"}}}}, now)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := s.ClaimReadyTask(ctx, "mer", "plan", "task", "attempt", domain.ScheduleLimits{}, now)
			if err != nil || claim.Reason != "" {
				t.Fatalf("claim %v %v", claim, err)
			}
			if tc != "unleased" {
				if ok, err := s.LeaseAttemptDispatch(ctx, "attempt", now); err != nil || !ok {
					t.Fatalf("lease %v %v", ok, err)
				}
			}
			rec := sampleRecord("mer")
			rec.Metadata = domain.SessionMetadata{}
			switch tc {
			case "project mismatch":
				rec.ProjectID = "other"
			case "kind mismatch":
				rec.Kind = domain.KindOrchestrator
			case "harness mismatch":
				rec.Harness = domain.HarnessCodex
			case "insert failure":
				rec.Activity.State = "invalid"
			}
			created, err := s.CreateTaskWorkerSession(ctx, rec, "attempt")
			view, ok, loadErr := s.LoadTaskSchedule(ctx, "mer", "plan")
			if loadErr != nil || !ok || len(view.Attempts) != 1 {
				t.Fatalf("view %v %v", ok, loadErr)
			}
			if tc != "success" {
				if err == nil || view.Attempts[0].SessionID != "" {
					t.Fatalf("failed creation leaked binding: %v %+v", err, view.Attempts[0])
				}
				sessions, err := s.ListSessions(ctx, "mer")
				if err != nil || len(sessions) != 0 {
					t.Fatalf("failed creation leaked session: %v %+v", err, sessions)
				}
				return
			}
			if err != nil || created.ID != "mer-1" || view.Attempts[0].SessionID != string(created.ID) {
				t.Fatalf("binding %v %+v %+v", err, created, view.Attempts[0])
			}
			if _, err := s.CreateTaskWorkerSession(ctx, rec, "attempt"); err == nil {
				t.Fatal("duplicate attempt launched")
			}
			if deleted, err := s.DeleteSession(ctx, created.ID); err == nil && deleted {
				t.Fatal("attempt identity lost on rollback")
			}
			if _, ok, err := s.TransitionTaskAttempt(ctx, "attempt", domain.TaskAttemptStateClaimed, domain.TaskAttemptStateFailed, nil, now); err != nil || !ok {
				t.Fatalf("finish attempt %v %v", ok, err)
			}
			if bound, err := s.IsTaskWorkerSession(ctx, created.ID); err != nil || !bound {
				t.Fatalf("terminal task lost ownership %v %v", bound, err)
			}
		})
	}
}

func TestClaimPinsResolvedHarnessAndCountsRealWorkerCapacity(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	project := domain.ProjectRecord{ID: "mer", Path: "/tmp/mer", RegisteredAt: now, Config: domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessCodex}}}
	if err := s.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateTaskPlan(ctx, domain.TaskPlan{ID: "plan", ProjectID: "mer", Title: "plan", Phases: []domain.TaskPhase{{ID: "phase", Title: "phase"}}, Tasks: []domain.PlannedTask{
		{ID: "default", WorkspaceKey: "default", PhaseID: "phase", Title: "default", Prompt: "work", VerificationCommands: []string{"true"}},
		{ID: "explicit", WorkspaceKey: "explicit", PhaseID: "phase", Title: "explicit", Prompt: "work", Harness: string(domain.HarnessCodex), VerificationCommands: []string{"true"}},
		{ID: "later-default", WorkspaceKey: "later-default", PhaseID: "phase", Title: "later", Prompt: "work", VerificationCommands: []string{"true"}},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	limits := domain.ScheduleLimits{PerProject: 2, PerHarness: 1}
	first, err := s.ClaimReadyTask(ctx, "mer", "plan", "default", "first", limits, now)
	if err != nil || first.Reason != "" || first.Attempt.Harness != domain.HarnessCodex {
		t.Fatalf("default claim %+v %v", first, err)
	}
	explicit, err := s.ClaimReadyTask(ctx, "mer", "plan", "explicit", "second", limits, now)
	if err != nil || explicit.Reason != domain.ClaimHarnessLimit {
		t.Fatalf("explicit bypassed default capacity %+v %v", explicit, err)
	}
	project.Config.Worker.Harness = domain.HarnessClaudeCode
	if err := s.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	explicit, err = s.ClaimReadyTask(ctx, "mer", "plan", "explicit", "second", limits, now)
	if err != nil || explicit.Reason != domain.ClaimHarnessLimit {
		t.Fatalf("default config change unpinned capacity %+v %v", explicit, err)
	}
	later, err := s.ClaimReadyTask(ctx, "mer", "plan", "later-default", "third", limits, now)
	if err != nil || later.Reason != "" || later.Attempt.Harness != domain.HarnessClaudeCode {
		t.Fatalf("new default claim %+v %v", later, err)
	}
}

func TestTaskWorkerSessionLookupMissing(t *testing.T) {
	s := newTestStore(t)
	bound, err := s.IsTaskWorkerSession(context.Background(), "missing")
	if err != nil || bound {
		t.Fatalf("missing ownership %v %v", bound, err)
	}
}
