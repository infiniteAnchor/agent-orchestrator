// Package tasksched derives a ready queue from durable task facts, claims work
// under project and harness limits, and reconciles attempts after a crash.
// Readiness is never stored. A verified result unlocks dependents even while
// the upstream worker is still alive. Process exit and Chat turn completion
// are not task results.
package tasksched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

const (
	// SignalExplicitResult is the only observation that may start verification.
	SignalExplicitResult = "explicit_result"
	// SignalProcessExit is recorded as ignored. A clean exit is not success.
	SignalProcessExit = "process_exit"
	// SignalTurnCompleted is the Chat bus event and is not a task result.
	SignalTurnCompleted = "turn_completed"

	defaultInterval = 2 * time.Second
)

// Store is the durable schedule surface. Implementations must run claim,
// launch, and result transitions atomically.
type Store interface {
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	ListSchedulePlans(context.Context) ([]domain.TaskPlanSummary, error)
	LoadTaskSchedule(context.Context, domain.ProjectID, string) (domain.TaskScheduleView, bool, error)
	ClaimReadyTask(context.Context, domain.ProjectID, string, string, string, domain.ScheduleLimits, time.Time) (domain.TaskClaimResult, error)
	LeaseAttemptDispatch(context.Context, string, time.Time) (bool, error)
	ReleaseAttemptDispatch(context.Context, string, time.Time) (bool, error)
	FinishAttemptLaunch(context.Context, string, string, string, domain.AgentHarness, time.Time) (domain.TaskAttempt, bool, error)
	HoldAttempt(context.Context, string, time.Time) (bool, error)
	FailAttemptLaunch(context.Context, string, time.Time) (bool, error)
	BeginCollection(context.Context, string, string, string, time.Time) (bool, error)
	CommitTaskResult(context.Context, domain.TaskResult) (bool, error)
	AlignTask(context.Context, string, string, time.Time) error
	GetTaskResultByAttempt(context.Context, string) (domain.TaskResult, bool, error)
}

// LaunchDisposition is what a launcher knows about one attempt.
type LaunchDisposition string

const (
	// LaunchStarted means this attempt's worker identity is known.
	LaunchStarted LaunchDisposition = "started"
	// LaunchNotStarted means the launcher confirmed no worker exists.
	LaunchNotStarted LaunchDisposition = "not_started"
	// LaunchAmbiguous means the launcher cannot tell. Callers must hold.
	LaunchAmbiguous LaunchDisposition = "ambiguous"
	// LaunchFailed means the launcher confirmed the worker never started.
	LaunchFailed LaunchDisposition = "failed"
)

// LaunchRequest identifies the attempt a launcher must use for idempotency.
type LaunchRequest struct {
	ProjectID    string
	PlanID       string
	TaskID       string
	AttemptID    string
	Prompt       string
	Harness      domain.AgentHarness
	WorkspaceKey string
}

// LaunchOutcome is a launcher decision. RuntimeRef and SessionID are opaque.
type LaunchOutcome struct {
	Disposition LaunchDisposition
	RuntimeRef  string
	SessionID   string
	Harness     domain.AgentHarness
}

// Launcher starts or adopts a worker for an attempt. Dispatch and Reconcile
// must be idempotent on AttemptID: a second call cannot start a second worker.
type Launcher interface {
	Dispatch(context.Context, LaunchRequest) (LaunchOutcome, error)
	Reconcile(context.Context, LaunchRequest) (LaunchOutcome, error)
}

// VerifyReport is the server-owned interpretation of verification commands.
type VerifyReport struct {
	Passed       bool
	Inconclusive bool
	Evidence     json.RawMessage
	Summary      string
}

// Verifier runs a task's declared commands. It must not treat a missing
// working directory or a failure to start as a failed task.
type Verifier interface {
	Verify(ctx context.Context, dir string, commands []string) (VerifyReport, error)
}

// Snapshot is the remote-safe schedule projection. It carries no prompts,
// commands, evidence, or host paths.
type Snapshot struct {
	Recovery     string
	ReadyTaskIDs []string
	Tasks        []TaskStatus
	Attempts     []AttemptStatus
}

// TaskStatus is one task's durable state plus derived readiness.
type TaskStatus struct {
	ID           string
	State        domain.TaskState
	WorkspaceKey string
	Harness      string
	Ready        bool
}

// AttemptStatus is one durable dispatch identity.
type AttemptStatus struct {
	ID            string
	TaskID        string
	AttemptNumber int
	State         domain.TaskAttemptState
	RuntimeRef    string
	SessionID     string
}

// DispatchReport lists attempts this call claimed or reconciled.
type DispatchReport struct {
	Recovery string
	Claims   []AttemptStatus
}

// Candidate is an explicit request to verify one running attempt.
type Candidate struct {
	AttemptID string `json:"attemptId" maxLength:"128"`
	Summary   string `json:"summary,omitempty" maxLength:"4096"`
	Signal    string `json:"signal" enum:"explicit_result,process_exit,turn_completed"`
}

// API is the HTTP-facing schedule surface.
type API interface {
	Ready() bool
	Snapshot(context.Context, domain.ProjectID, string) (Snapshot, error)
	Dispatch(context.Context, domain.ProjectID, string) (DispatchReport, error)
	SubmitCandidate(context.Context, domain.ProjectID, string, string, Candidate) (Completion, error)
}

// Completion is the durable outcome of a candidate. Summary text stays in the
// result row and is not part of this projection.
type Completion struct {
	AttemptID string
	TaskID    string
	Outcome   domain.TaskResultOutcome
	TaskState domain.TaskState
}

// Deps configures the scheduler.
type Deps struct {
	Store    Store
	Launcher Launcher
	Verifier Verifier
	Limits   domain.ScheduleLimits
	Clock    func() time.Time
	NewID    func() string
	Interval time.Duration
}

// Scheduler claims ready work and recovers attempts after a restart.
type Scheduler struct {
	store    Store
	launcher Launcher
	verifier Verifier
	limits   domain.ScheduleLimits
	now      func() time.Time
	newID    func() string
	interval time.Duration

	mu    sync.Mutex
	ready atomic.Bool
	seq   uint64
}

var _ API = (*Scheduler)(nil)

// New constructs a scheduler. It is not ready until Recover succeeds, so
// dispatch cannot run ahead of reconciliation.
func New(store Store, launcher Launcher, verifier Verifier) *Scheduler {
	return NewWithDeps(Deps{Store: store, Launcher: launcher, Verifier: verifier})
}

// NewWithDeps constructs a scheduler with explicit dependencies.
func NewWithDeps(d Deps) *Scheduler {
	now := d.Clock
	if now == nil {
		now = time.Now
	}
	newID := d.NewID
	if newID == nil {
		newID = func() string { return uuid.NewString() }
	}
	interval := d.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	return &Scheduler{
		store: d.Store, launcher: d.Launcher, verifier: d.Verifier,
		limits: d.Limits.Normalized(), now: now, newID: newID, interval: interval,
	}
}

// Ready reports whether startup reconciliation has finished. Dispatch stays
// closed until it has, which is what /readyz exposes as taskRecovery.
func (s *Scheduler) Ready() bool {
	return s != nil && s.ready.Load()
}

// Run reconciles once, then dispatches until ctx is cancelled. A failed
// recovery leaves the scheduler unready so a later tick cannot launch a
// replacement for an attempt whose launch is still ambiguous.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.Recover(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := s.DispatchAll(ctx); err != nil && ctx.Err() == nil {
				return err
			}
		}
	}
}

// Recover adopts recorded launches, holds ambiguous ones, and resumes
// verification that was already in progress. It does not claim new work.
func (s *Scheduler) Recover(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store == nil || s.launcher == nil || s.verifier == nil {
		return apierr.Internal("TASK_SCHEDULER_UNAVAILABLE", "Task scheduler is unavailable")
	}
	plans, err := s.store.ListSchedulePlans(ctx)
	if err != nil {
		return err
	}
	for _, plan := range plans {
		if err := s.recoverPlan(ctx, domain.ProjectID(plan.ProjectID), plan.ID); err != nil {
			return err
		}
	}
	s.ready.Store(true)
	return nil
}

func (s *Scheduler) recoverPlan(ctx context.Context, projectID domain.ProjectID, planID string) error {
	view, ok, err := s.store.LoadTaskSchedule(ctx, projectID, planID)
	if err != nil || !ok {
		return err
	}
	for _, attempt := range view.Attempts {
		if attempt.State.Terminal() {
			continue
		}
		if err := s.reconcile(ctx, view, attempt, true); err != nil {
			return err
		}
	}
	view, ok, err = s.store.LoadTaskSchedule(ctx, projectID, planID)
	if err != nil || !ok {
		return err
	}
	for _, node := range view.Nodes {
		if err := s.store.AlignTask(ctx, planID, node.ID, s.now().UTC()); err != nil {
			return err
		}
	}
	for _, attempt := range view.Attempts {
		if attempt.State != domain.TaskAttemptStateCollecting {
			continue
		}
		if _, found, err := s.store.GetTaskResultByAttempt(ctx, attempt.ID); err != nil {
			return err
		} else if found {
			continue
		}
		if _, err := s.verifyAndCommit(ctx, view, attempt, ""); err != nil {
			return err
		}
	}
	return nil
}

// DispatchAll claims ready work in every plan. It is a no-op until Recover
// has finished.
func (s *Scheduler) DispatchAll(ctx context.Context) ([]DispatchReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireReady(); err != nil {
		return nil, err
	}
	plans, err := s.store.ListSchedulePlans(ctx)
	if err != nil {
		return nil, apierr.Internal("TASK_SCHEDULE_LOAD_FAILED", "Failed to load task plans")
	}
	reports := make([]DispatchReport, 0, len(plans))
	for _, plan := range plans {
		report, err := s.dispatchLocked(ctx, domain.ProjectID(plan.ProjectID), plan.ID)
		if err != nil {
			var api *apierr.Error
			if errors.As(err, &api) && api.Code == "TASK_PLAN_NOT_FOUND" {
				continue
			}
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

// Dispatch claims ready tasks in one plan and launches each new attempt.
func (s *Scheduler) Dispatch(ctx context.Context, projectID domain.ProjectID, planID string) (DispatchReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireReady(); err != nil {
		return DispatchReport{}, err
	}
	if _, err := s.requireProject(ctx, projectID); err != nil {
		return DispatchReport{}, err
	}
	return s.dispatchLocked(ctx, projectID, planID)
}

func (s *Scheduler) dispatchLocked(ctx context.Context, projectID domain.ProjectID, planID string) (DispatchReport, error) {
	view, ok, err := s.store.LoadTaskSchedule(ctx, projectID, planID)
	if err != nil {
		return DispatchReport{}, apierr.Internal("TASK_SCHEDULE_LOAD_FAILED", "Failed to load task schedule")
	}
	if !ok {
		return DispatchReport{}, apierr.NotFound("TASK_PLAN_NOT_FOUND", "Unknown task plan")
	}
	var claims []AttemptStatus
	for _, attempt := range view.Attempts {
		if attempt.State != domain.TaskAttemptStateClaimed || attempt.RuntimeRef != "" {
			continue
		}
		status, err := s.launch(ctx, view, attempt)
		if err != nil {
			return DispatchReport{}, err
		}
		if status.ID != "" {
			claims = append(claims, status)
		}
	}
	for {
		view, ok, err = s.store.LoadTaskSchedule(ctx, projectID, planID)
		if err != nil || !ok {
			if err == nil {
				err = apierr.NotFound("TASK_PLAN_NOT_FOUND", "Unknown task plan")
			}
			return DispatchReport{}, err
		}
		ready := domain.DerivedReady(view.Nodes, view.Verified)
		if len(ready) == 0 {
			break
		}
		var claimed *domain.TaskAttempt
		for _, taskID := range ready {
			result, err := s.store.ClaimReadyTask(ctx, projectID, planID, taskID, s.nextID(), s.limits, s.now().UTC())
			if err != nil {
				return DispatchReport{}, apierr.Internal("TASK_CLAIM_FAILED", "Failed to claim task")
			}
			if result.Reason == "" {
				copy := result.Attempt
				claimed = &copy
				break
			}
		}
		if claimed == nil {
			break
		}
		status, err := s.launch(ctx, view, *claimed)
		if err != nil {
			return DispatchReport{}, err
		}
		if status.ID != "" {
			claims = append(claims, status)
		}
	}
	if claims == nil {
		claims = []AttemptStatus{}
	}
	return DispatchReport{Recovery: "complete", Claims: claims}, nil
}

// Snapshot returns the derived schedule for one plan.
func (s *Scheduler) Snapshot(ctx context.Context, projectID domain.ProjectID, planID string) (Snapshot, error) {
	if s == nil || s.store == nil {
		return Snapshot{}, apierr.Internal("TASK_SCHEDULER_UNAVAILABLE", "Task scheduler is unavailable")
	}
	if _, err := s.requireProject(ctx, projectID); err != nil {
		return Snapshot{}, err
	}
	view, ok, err := s.store.LoadTaskSchedule(ctx, projectID, planID)
	if err != nil {
		return Snapshot{}, apierr.Internal("TASK_SCHEDULE_LOAD_FAILED", "Failed to load task schedule")
	}
	if !ok {
		return Snapshot{}, apierr.NotFound("TASK_PLAN_NOT_FOUND", "Unknown task plan")
	}
	return snapshotOf(view, s.ready.Load()), nil
}

// SubmitCandidate verifies an explicit result. Process exit and turn
// completion do not change durable task state.
func (s *Scheduler) SubmitCandidate(ctx context.Context, projectID domain.ProjectID, planID, taskID string, in Candidate) (Completion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireReady(); err != nil {
		return Completion{}, err
	}
	switch in.Signal {
	case SignalProcessExit:
		return Completion{}, apierr.Conflict("TASK_SIGNAL_IGNORED", "Process exit does not complete a task", nil)
	case SignalTurnCompleted:
		return Completion{}, apierr.Conflict("TASK_SIGNAL_IGNORED", "Chat turn completion does not complete a task", nil)
	case SignalExplicitResult:
	default:
		return Completion{}, apierr.Invalid("INVALID_TASK_SIGNAL", "Task result signal must be explicit_result", nil)
	}
	if len(in.AttemptID) == 0 || len(in.AttemptID) > 128 || in.AttemptID != strings.TrimSpace(in.AttemptID) {
		return Completion{}, apierr.Invalid("INVALID_TASK_ATTEMPT", "Task attempt id is invalid", nil)
	}
	if len(in.Summary) > 4096 {
		return Completion{}, apierr.Invalid("TASK_RESULT_TOO_LARGE", "Task result summary exceeds 4096 bytes", map[string]any{"field": "summary", "maxBytes": 4096})
	}
	if _, err := s.requireProject(ctx, projectID); err != nil {
		return Completion{}, err
	}
	view, ok, err := s.store.LoadTaskSchedule(ctx, projectID, planID)
	if err != nil {
		return Completion{}, apierr.Internal("TASK_SCHEDULE_LOAD_FAILED", "Failed to load task schedule")
	}
	if !ok {
		return Completion{}, apierr.NotFound("TASK_PLAN_NOT_FOUND", "Unknown task plan")
	}
	var attempt domain.TaskAttempt
	found := false
	for _, candidate := range view.Attempts {
		if candidate.ID == in.AttemptID && candidate.TaskID == taskID {
			attempt = candidate
			found = true
			break
		}
	}
	if !found {
		return Completion{}, apierr.NotFound("TASK_ATTEMPT_NOT_FOUND", "Unknown task attempt")
	}
	if attempt.State != domain.TaskAttemptStateRunning && attempt.State != domain.TaskAttemptStateCollecting {
		return Completion{}, apierr.Conflict("TASK_ATTEMPT_NOT_COLLECTABLE", "Task attempt is not collecting a result", nil)
	}
	if attempt.State == domain.TaskAttemptStateRunning {
		applied, err := s.store.BeginCollection(ctx, planID, taskID, attempt.ID, s.now().UTC())
		if err != nil || !applied {
			if err == nil {
				err = apierr.Conflict("TASK_ATTEMPT_NOT_COLLECTABLE", "Task attempt is not collecting a result", nil)
			}
			return Completion{}, err
		}
	}
	return s.verifyAndCommit(ctx, view, attempt, in.Summary)
}

func (s *Scheduler) verifyAndCommit(ctx context.Context, view domain.TaskScheduleView, attempt domain.TaskAttempt, summary string) (Completion, error) {
	dir := ""
	record, ok, err := s.store.GetProject(ctx, view.ProjectID)
	if err != nil {
		return Completion{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if ok {
		dir = record.Path
	}
	var commands []string
	for _, task := range view.Plan.Tasks {
		if task.ID == attempt.TaskID {
			commands = append([]string(nil), task.VerificationCommands...)
			break
		}
	}
	report, err := s.verifier.Verify(ctx, dir, commands)
	if err != nil {
		return Completion{}, apierr.Internal("TASK_VERIFY_FAILED", "Failed to verify task")
	}
	outcome := domain.TaskResultVerified
	switch {
	case report.Inconclusive:
		outcome = domain.TaskResultInconclusive
	case !report.Passed:
		outcome = domain.TaskResultFailed
	}
	if len(report.Evidence) == 0 {
		report.Evidence = json.RawMessage(`{"passed":false,"steps":[]}`)
	}
	result := domain.TaskResult{
		ID: s.nextID(), PlanID: attempt.PlanID, TaskID: attempt.TaskID, AttemptID: attempt.ID,
		Outcome: outcome, Summary: summary, Evidence: report.Evidence, RecordedAt: s.now().UTC(),
	}
	if summary == "" {
		result.Summary = report.Summary
	}
	applied, err := s.store.CommitTaskResult(ctx, result)
	if err != nil || !applied {
		if err == nil {
			err = apierr.Conflict("TASK_RESULT_CONFLICT", "Task result was already recorded", nil)
		}
		return Completion{}, err
	}
	return Completion{
		AttemptID: attempt.ID, TaskID: attempt.TaskID, Outcome: outcome, TaskState: taskStateForOutcome(outcome),
	}, nil
}

func (s *Scheduler) reconcile(ctx context.Context, view domain.TaskScheduleView, attempt domain.TaskAttempt, recovery bool) error {
	switch attempt.State {
	case domain.TaskAttemptStateBlocked, domain.TaskAttemptStateCollecting:
		// Collecting is resumed from SQLite by re-running verification. Holding
		// it would treat an in-progress result as an ambiguous launch.
		return nil
	case domain.TaskAttemptStateRunning:
		if attempt.RuntimeRef == "" || attempt.RuntimeRef == domain.TaskAttemptDispatchLease {
			_, err := s.store.HoldAttempt(ctx, attempt.ID, s.now().UTC())
			return err
		}
		return s.store.AlignTask(ctx, attempt.PlanID, attempt.TaskID, s.now().UTC())
	case domain.TaskAttemptStateClaimed:
		if attempt.RuntimeRef == "" && !recovery {
			return nil
		}
		if attempt.RuntimeRef == "" || attempt.RuntimeRef == domain.TaskAttemptDispatchLease {
			_, err := s.launch(ctx, view, attempt)
			return err
		}
		_, _, err := s.store.FinishAttemptLaunch(ctx, attempt.ID, attempt.RuntimeRef, attempt.SessionID, attempt.Harness, s.now().UTC())
		return err
	default:
		return nil
	}
}

func (s *Scheduler) launch(ctx context.Context, view domain.TaskScheduleView, attempt domain.TaskAttempt) (AttemptStatus, error) {
	req := launchRequest(view, attempt)
	// A lease that is already durable means a previous process may have launched.
	// Reconcile that attempt. A new claim still has an empty ref: lease it and
	// dispatch, which is the only path that is allowed to start a worker.
	reconcileLease := attempt.RuntimeRef == domain.TaskAttemptDispatchLease
	if attempt.RuntimeRef == "" {
		leased, err := s.store.LeaseAttemptDispatch(ctx, attempt.ID, s.now().UTC())
		if err != nil {
			return AttemptStatus{}, err
		}
		if !leased {
			return AttemptStatus{}, nil
		}
		attempt.RuntimeRef = domain.TaskAttemptDispatchLease
	}
	var outcome LaunchOutcome
	var err error
	switch {
	case reconcileLease:
		outcome, err = s.launcher.Reconcile(ctx, req)
		if err != nil || outcome.Disposition == LaunchAmbiguous || outcome.Disposition == "" {
			_, holdErr := s.store.HoldAttempt(ctx, attempt.ID, s.now().UTC())
			if err != nil {
				return AttemptStatus{}, err
			}
			return AttemptStatus{}, holdErr
		}
		if outcome.Disposition == LaunchNotStarted {
			if _, err := s.store.ReleaseAttemptDispatch(ctx, attempt.ID, s.now().UTC()); err != nil {
				return AttemptStatus{}, err
			}
			if _, err := s.store.LeaseAttemptDispatch(ctx, attempt.ID, s.now().UTC()); err != nil {
				return AttemptStatus{}, err
			}
			outcome, err = s.launcher.Dispatch(ctx, req)
			if err != nil {
				_, _ = s.store.HoldAttempt(ctx, attempt.ID, s.now().UTC())
				return AttemptStatus{}, err
			}
		}
	case attempt.RuntimeRef == domain.TaskAttemptDispatchLease:
		outcome, err = s.launcher.Dispatch(ctx, req)
		if err != nil {
			_, _ = s.store.HoldAttempt(ctx, attempt.ID, s.now().UTC())
			return AttemptStatus{}, err
		}
	default:
		outcome = LaunchOutcome{Disposition: LaunchStarted, RuntimeRef: attempt.RuntimeRef, SessionID: attempt.SessionID, Harness: attempt.Harness}
	}
	return s.applyLaunch(ctx, attempt, outcome)
}

func (s *Scheduler) applyLaunch(ctx context.Context, attempt domain.TaskAttempt, outcome LaunchOutcome) (AttemptStatus, error) {
	at := s.now().UTC()
	switch outcome.Disposition {
	case LaunchStarted:
		if err := domain.ValidateRuntimeRef(outcome.RuntimeRef); err != nil || outcome.RuntimeRef == "" || outcome.RuntimeRef == domain.TaskAttemptDispatchLease {
			_, err := s.store.HoldAttempt(ctx, attempt.ID, at)
			return AttemptStatus{}, err
		}
		harness := outcome.Harness
		if harness == "" {
			harness = attempt.Harness
		}
		updated, applied, err := s.store.FinishAttemptLaunch(ctx, attempt.ID, outcome.RuntimeRef, outcome.SessionID, harness, at)
		if err != nil || !applied {
			_, _ = s.store.HoldAttempt(ctx, attempt.ID, at)
			if err != nil {
				return AttemptStatus{}, err
			}
			return AttemptStatus{}, apierr.Conflict("TASK_LAUNCH_CONFLICT", "Task launch could not be recorded", nil)
		}
		return statusOf(updated), nil
	case LaunchNotStarted:
		_, err := s.store.ReleaseAttemptDispatch(ctx, attempt.ID, at)
		return AttemptStatus{}, err
	case LaunchFailed:
		_, err := s.store.FailAttemptLaunch(ctx, attempt.ID, at)
		if err != nil {
			return AttemptStatus{}, err
		}
		attempt.State = domain.TaskAttemptStateFailed
		return statusOf(attempt), nil
	default:
		_, err := s.store.HoldAttempt(ctx, attempt.ID, at)
		if err != nil {
			return AttemptStatus{}, err
		}
		attempt.State = domain.TaskAttemptStateBlocked
		return statusOf(attempt), nil
	}
}

func (s *Scheduler) requireReady() error {
	if s == nil || s.store == nil || s.launcher == nil || s.verifier == nil {
		return apierr.Internal("TASK_SCHEDULER_UNAVAILABLE", "Task scheduler is unavailable")
	}
	if !s.ready.Load() {
		return apierr.Unavailable("TASK_RECOVERY_PENDING", "Task recovery is still running")
	}
	return nil
}

func (s *Scheduler) requireProject(ctx context.Context, projectID domain.ProjectID) (domain.ProjectRecord, error) {
	record, ok, err := s.store.GetProject(ctx, string(projectID))
	if err != nil {
		return domain.ProjectRecord{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok || !record.ArchivedAt.IsZero() {
		return domain.ProjectRecord{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	return record, nil
}

func (s *Scheduler) nextID() string {
	s.seq++
	id := s.newID()
	if id == "" {
		return fmt.Sprintf("attempt-%d", s.seq)
	}
	return id
}

func launchRequest(view domain.TaskScheduleView, attempt domain.TaskAttempt) LaunchRequest {
	req := LaunchRequest{
		ProjectID: view.ProjectID, PlanID: attempt.PlanID, TaskID: attempt.TaskID,
		AttemptID: attempt.ID, Harness: attempt.Harness,
	}
	for _, task := range view.Plan.Tasks {
		if task.ID == attempt.TaskID {
			req.Prompt = task.Prompt
			req.WorkspaceKey = task.WorkspaceKey
			if req.Harness == "" {
				req.Harness = domain.AgentHarness(task.Harness)
			}
			break
		}
	}
	return req
}

func snapshotOf(view domain.TaskScheduleView, ready bool) Snapshot {
	readyIDs := domain.DerivedReady(view.Nodes, view.Verified)
	readySet := make(map[string]struct{}, len(readyIDs))
	for _, id := range readyIDs {
		readySet[id] = struct{}{}
	}
	tasks := make([]TaskStatus, 0, len(view.Nodes))
	for _, node := range view.Nodes {
		_, isReady := readySet[node.ID]
		tasks = append(tasks, TaskStatus{
			ID: node.ID, State: node.State, WorkspaceKey: node.WorkspaceKey, Harness: node.Harness, Ready: isReady,
		})
	}
	attempts := make([]AttemptStatus, 0, len(view.Attempts))
	for _, attempt := range view.Attempts {
		attempts = append(attempts, statusOf(attempt))
	}
	recovery := "pending"
	if ready {
		recovery = "complete"
	}
	return Snapshot{Recovery: recovery, ReadyTaskIDs: readyIDs, Tasks: tasks, Attempts: attempts}
}

func statusOf(attempt domain.TaskAttempt) AttemptStatus {
	ref := attempt.RuntimeRef
	if ref == domain.TaskAttemptDispatchLease {
		ref = ""
	}
	return AttemptStatus{
		ID: attempt.ID, TaskID: attempt.TaskID, AttemptNumber: attempt.AttemptNumber,
		State: attempt.State, RuntimeRef: ref, SessionID: attempt.SessionID,
	}
}

func taskStateForOutcome(outcome domain.TaskResultOutcome) domain.TaskState {
	switch outcome {
	case domain.TaskResultVerified:
		return domain.TaskStateCompleted
	case domain.TaskResultFailed:
		return domain.TaskStateFailed
	case domain.TaskResultInconclusive:
		return domain.TaskStateBlocked
	default:
		return ""
	}
}
