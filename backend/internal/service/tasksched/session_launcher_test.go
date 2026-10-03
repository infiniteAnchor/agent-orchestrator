package tasksched

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type fakeWorkerSessions struct {
	db    *store.Store
	calls int
	crash string
	dir   string
}

func (f *fakeWorkerSessions) Spawn(ctx context.Context, cfg ports.SpawnConfig) (domain.Session, int, int, error) {
	f.calls++
	if f.crash == "before_create" {
		return domain.Session{}, 0, 0, errors.New("crash")
	}
	rec, err := f.db.CreateTaskWorkerSession(ctx, domain.SessionRecord{
		ProjectID: cfg.ProjectID, Kind: cfg.Kind, Harness: cfg.Harness, Mode: domain.SessionModeTUI,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, cfg.TaskAttemptID)
	if err != nil {
		return domain.Session{}, 0, 0, err
	}
	if f.crash == "after_create" {
		return domain.Session{}, 0, 0, errors.New("crash")
	}
	rec.Metadata.WorkspacePath = f.dir
	rec.Metadata.RuntimeHandleID = "handle"
	rec.Metadata.Prompt = cfg.Prompt
	if err := f.db.UpdateSession(ctx, rec); err != nil {
		return domain.Session{}, 0, 0, err
	}
	return domain.Session{SessionRecord: rec}, 0, 0, nil
}
func (f *fakeWorkerSessions) WorkspaceLocation(ctx context.Context, id domain.SessionID) (string, error) {
	rec, ok, err := f.db.GetSession(ctx, id)
	if err != nil || !ok || rec.Metadata.WorkspacePath == "" {
		return "", errors.New("workspace unavailable")
	}
	return rec.Metadata.WorkspacePath, nil
}

type fakeWorkerProbe struct {
	alive bool
	err   error
}

func (p fakeWorkerProbe) TaskWorkerAlive(context.Context, domain.SessionID) (bool, error) {
	return p.alive, p.err
}

func TestSessionLauncherCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"before_create", "after_create", "before_record", "unknown_probe"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			db := sqlitetest.MustOpen(t)
			seedProject(t, db, "proj")
			plan := domain.TaskPlan{ID: "plan", ProjectID: "proj", Title: "Plan", Tasks: []domain.PlannedTask{{ID: "a", Title: "A", Prompt: "work", Harness: string(domain.HarnessCodex)}}}
			now := time.Now()
			if _, err := db.CreateTaskPlan(ctx, plan, now); err != nil {
				t.Fatal(err)
			}
			claim, err := db.ClaimReadyTask(ctx, "proj", "plan", "a", "attempt", domain.ScheduleLimits{}, now)
			if err != nil || claim.Reason != "" {
				t.Fatalf("claim: %+v %v", claim, err)
			}
			if ok, err := db.LeaseAttemptDispatch(ctx, "attempt", now); err != nil || !ok {
				t.Fatalf("lease: %v %v", ok, err)
			}
			sessions := &fakeWorkerSessions{db: db, dir: t.TempDir()}
			probe := fakeWorkerProbe{alive: true}
			if boundary == "before_create" || boundary == "after_create" {
				sessions.crash = boundary
			}
			if boundary == "unknown_probe" {
				probe.err = errors.New("runtime unknown")
			}
			req := LaunchRequest{ProjectID: "proj", PlanID: "plan", TaskID: "a", AttemptID: "attempt", Harness: domain.HarnessCodex, Prompt: "work"}
			launcher := NewSessionLauncher(db, sessions, probe)
			outcome, _ := launcher.Dispatch(ctx, req)
			if boundary == "before_record" && outcome.Disposition != LaunchStarted {
				t.Fatalf("launch: %+v", outcome)
			}
			// Replace the adapter and recover using only durable SQLite ownership.
			sessions.crash = ""
			launcher = NewSessionLauncher(db, sessions, probe)
			sched := schedulerOn(t, db, launcher)
			if err := sched.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			attempts, err := db.ListTaskAttempts(ctx, "plan", "a")
			if err != nil || len(attempts) != 1 {
				t.Fatalf("attempts: %+v %v", attempts, err)
			}
			want := domain.TaskAttemptStateRunning
			if boundary == "after_create" || boundary == "unknown_probe" {
				want = domain.TaskAttemptStateBlocked
			}
			if attempts[0].State != want {
				t.Fatalf("state: %+v want %s", attempts[0], want)
			}
			report, err := sched.Dispatch(ctx, "proj", "plan")
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Claims) != 0 {
				t.Fatalf("repeat claimed: %+v", report)
			}
			expectedCalls := 1
			if boundary == "before_create" {
				expectedCalls = 2
			}
			if sessions.calls != expectedCalls {
				t.Fatalf("spawn calls %d want %d", sessions.calls, expectedCalls)
			}
			if want == domain.TaskAttemptStateRunning && attempts[0].SessionID == "" {
				t.Fatal("real launch missing session association")
			}
		})
	}
}

func TestSessionLauncherRepeatedDispatchUsesBoundSession(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	seedProject(t, db, "proj")
	plan := domain.TaskPlan{ID: "plan", ProjectID: "proj", Title: "Plan", Tasks: []domain.PlannedTask{{ID: "a", Title: "A", Prompt: "work", Harness: string(domain.HarnessCodex)}}}
	if _, err := db.CreateTaskPlan(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}
	sessions := &fakeWorkerSessions{db: db, dir: t.TempDir()}
	launcher := NewSessionLauncher(db, sessions, fakeWorkerProbe{alive: true})
	sched := schedulerOn(t, db, launcher)
	if err := sched.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	report, err := sched.Dispatch(ctx, "proj", "plan")
	if err != nil || len(report.Claims) != 1 {
		t.Fatalf("dispatch: %+v %v", report, err)
	}
	claim := report.Claims[0]
	req := LaunchRequest{ProjectID: "proj", PlanID: "plan", TaskID: "a", AttemptID: claim.ID, Harness: domain.HarnessCodex}
	for i := 0; i < 2; i++ {
		next, err := NewSessionLauncher(db, sessions, fakeWorkerProbe{alive: true}).Dispatch(ctx, req)
		if err != nil || next.SessionID != claim.SessionID || next.RuntimeRef != claim.RuntimeRef {
			t.Fatalf("adopt: %+v %v", next, err)
		}
	}
	if sessions.calls != 1 {
		t.Fatalf("duplicate spawns: %d", sessions.calls)
	}
}

// A lost launch-record write cannot erase the atomic session association or
// authorize a replacement, even though the caller saw an error.
type lostLaunchRecordStore struct{ Store }

func (s lostLaunchRecordStore) FinishAttemptLaunch(context.Context, string, string, string, domain.AgentHarness, time.Time) (domain.TaskAttempt, bool, error) {
	return domain.TaskAttempt{}, false, errors.New("launch record write lost")
}

func TestSessionLauncherLostLaunchRecordingHoldsBoundWorker(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpen(t)
	seedProject(t, db, "proj")
	plan := domain.TaskPlan{ID: "plan", ProjectID: "proj", Title: "Plan", Tasks: []domain.PlannedTask{{ID: "a", Title: "A", Prompt: "work", Harness: string(domain.HarnessCodex)}}}
	if _, err := db.CreateTaskPlan(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}
	sessions := &fakeWorkerSessions{db: db, dir: t.TempDir()}
	launcher := NewSessionLauncher(db, sessions, fakeWorkerProbe{alive: true})
	sched := New(lostLaunchRecordStore{Store: db}, launcher, ExecVerifier{})
	if err := sched.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sched.Dispatch(ctx, "proj", "plan"); err == nil {
		t.Fatal("expected launch recording error")
	}
	attempts, err := db.ListTaskAttempts(ctx, "plan", "a")
	if err != nil || len(attempts) != 1 || attempts[0].State != domain.TaskAttemptStateBlocked || attempts[0].SessionID == "" {
		t.Fatalf("lost binding: %+v %v", attempts, err)
	}
	// Recover from the real durable store, with a fresh adapter instance.
	restored := schedulerOn(t, db, NewSessionLauncher(db, sessions, fakeWorkerProbe{alive: true}))
	if err := restored.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if report, err := restored.Dispatch(ctx, "proj", "plan"); err != nil || len(report.Claims) != 0 {
		t.Fatalf("held attempt replaced: %+v %v", report, err)
	}
	if sessions.calls != 1 {
		t.Fatalf("worker launched %d times", sessions.calls)
	}
}
