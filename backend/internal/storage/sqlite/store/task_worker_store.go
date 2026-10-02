package store

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CreateTaskWorkerSession publishes the session seed and attempt binding in one
// transaction, before the manager creates any workspace or runtime. An existing
// binding always refuses another launch, including an incomplete seed after a crash.
func (s *Store) CreateTaskWorkerSession(ctx context.Context, rec domain.SessionRecord, attemptID string) (domain.SessionRecord, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.SessionRecord{}, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "create task worker session", func(q *gen.Queries) error {
		attempt, err := q.GetTaskAttempt(ctx, attemptID)
		if err != nil {
			return err
		}
		owner, err := q.ActiveProjectForPlan(ctx, attempt.PlanID)
		if err != nil {
			return err
		}
		if owner != rec.ProjectID || rec.Kind != domain.KindWorker || rec.IsTerminated || rec.Harness == "" {
			return fmt.Errorf("task attempt %s worker identity mismatch", attemptID)
		}
		if attempt.State != domain.TaskAttemptStateClaimed || attempt.RuntimeRef != domain.TaskAttemptDispatchLease || attempt.SessionID != nil {
			return fmt.Errorf("task attempt %s is not an unbound leased claim", attemptID)
		}
		if attempt.Harness != nil && *attempt.Harness != "" && *attempt.Harness != rec.Harness {
			return fmt.Errorf("task attempt %s harness mismatch", attemptID)
		}
		num, err := q.NextSessionNum(ctx, rec.ProjectID)
		if err != nil {
			return err
		}
		rec.ID = domain.SessionID(fmt.Sprintf("%s-%d", rec.ProjectID, num))
		if err := q.InsertSession(ctx, recordToInsert(rec, num)); err != nil {
			return err
		}
		rows, err := q.BindTaskWorkerSession(ctx, gen.BindTaskWorkerSessionParams{SessionID: nullableSessionID(string(rec.ID)), Harness: nullableHarness(rec.Harness), UpdatedAt: rec.UpdatedAt, ID: attemptID})
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("task attempt %s binding changed", attemptID)
		}
		return nil
	})
	if err != nil {
		return domain.SessionRecord{}, err
	}
	return rec, nil
}

// IsTaskWorkerSession includes completed attempts: daemon recovery must never
// replay a finished task's provider prompt either.
func (s *Store) IsTaskWorkerSession(ctx context.Context, id domain.SessionID) (bool, error) {
	bound, err := s.qr.IsTaskWorkerSession(ctx, nullableSessionID(string(id)))
	return bound, err
}
