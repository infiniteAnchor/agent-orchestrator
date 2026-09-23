package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

const proposalTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

func proposalTimestamp(t time.Time) string { return t.UTC().Format(proposalTimestampLayout) }

// CreateTaskPlanProposal is idempotent on the project-scoped request key.
func (s *Store) CreateTaskPlanProposal(ctx context.Context, proposal domain.TaskPlanProposal) (domain.TaskPlanProposal, bool, error) {
	if proposal.ID == "" || proposal.RequestKey == "" || proposal.ProjectID == "" || proposal.CreatedAt.IsZero() {
		return domain.TaskPlanProposal{}, false, errors.New("create task plan proposal: required identity and timestamp")
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.TaskPlanProposal{}, false, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "create task plan proposal", func(q *gen.Queries) error {
		inserted, err := q.InsertTaskPlanProposal(ctx, gen.InsertTaskPlanProposalParams{
			ID: proposal.ID, ProjectID: proposal.ProjectID, RequestKey: proposal.RequestKey,
			Specification: proposal.Specification, Status: string(domain.TaskPlanProposalQueued),
			CreatedAt: proposalTimestamp(proposal.CreatedAt),
			UpdatedAt: proposalTimestamp(proposal.CreatedAt),
		})
		if err != nil {
			return err
		}
		_, err = q.GetTaskPlanProposalByRequest(ctx, gen.GetTaskPlanProposalByRequestParams{
			ProjectID: string(proposal.ProjectID), RequestKey: proposal.RequestKey,
		})
		if errors.Is(err, sql.ErrNoRows) && inserted == 0 {
			return domain.ErrTaskPlanProposalProjectNotFound
		}
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.TaskPlanProposal{}, false, err
	}
	got, _, err := s.GetTaskPlanProposalByRequest(ctx, proposal.ProjectID, proposal.RequestKey)
	if err != nil {
		return domain.TaskPlanProposal{}, false, err
	}
	return got, got.ID == proposal.ID, nil
}

func (s *Store) GetTaskPlanProposalByRequest(ctx context.Context, projectID domain.ProjectID, requestKey string) (domain.TaskPlanProposal, bool, error) {
	row, err := s.qr.GetTaskPlanProposalByRequest(ctx, gen.GetTaskPlanProposalByRequestParams{ProjectID: string(projectID), RequestKey: requestKey})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskPlanProposal{}, false, nil
	}
	if err != nil {
		return domain.TaskPlanProposal{}, false, err
	}
	proposal, err := taskPlanProposalFromRow(row)
	return proposal, err == nil, err
}

func (s *Store) GetTaskPlanProposal(ctx context.Context, projectID domain.ProjectID, id string) (domain.TaskPlanProposal, bool, error) {
	row, err := s.qr.GetTaskPlanProposal(ctx, gen.GetTaskPlanProposalParams{ProjectID: string(projectID), ID: id})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.TaskPlanProposal{}, false, nil
	}
	if err != nil {
		return domain.TaskPlanProposal{}, false, err
	}
	proposal, err := taskPlanProposalFromRow(row)
	return proposal, err == nil, err
}

func (s *Store) ListTaskPlanProposals(ctx context.Context, projectID domain.ProjectID, limit int64) ([]domain.TaskPlanProposal, error) {
	rows, err := s.qr.ListTaskPlanProposals(ctx, gen.ListTaskPlanProposalsParams{ProjectID: string(projectID), Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]domain.TaskPlanProposal, 0, len(rows))
	for _, row := range rows {
		proposal, err := taskPlanProposalFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, proposal)
	}
	return out, nil
}

func (s *Store) ListPendingTaskPlanProposals(ctx context.Context) ([]domain.TaskPlanProposal, error) {
	rows, err := s.qr.ListPendingTaskPlanProposals(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.TaskPlanProposal, 0, len(rows))
	for _, row := range rows {
		proposal, err := taskPlanProposalFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, proposal)
	}
	return out, nil
}

// SetTaskPlanProposalState compare-and-swaps one durable proposal transition.
func (s *Store) SetTaskPlanProposalState(ctx context.Context, proposal domain.TaskPlanProposal, expected domain.TaskPlanProposalState, now time.Time) (bool, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	var acceptedAt, rejectedAt sql.NullString
	if proposal.AcceptedAt != nil {
		acceptedAt = sql.NullString{String: proposalTimestamp(*proposal.AcceptedAt), Valid: true}
	}
	if proposal.RejectedAt != nil {
		rejectedAt = sql.NullString{String: proposalTimestamp(*proposal.RejectedAt), Valid: true}
	}
	rows, err := s.qw.SetTaskPlanProposalState(ctx, gen.SetTaskPlanProposalStateParams{
		Status: string(proposal.State), OrchestratorID: string(proposal.OrchestratorID), TurnID: proposal.TurnID,
		GraphJson: proposal.GraphJSON, ErrorCode: proposal.ErrorCode, ErrorMessage: proposal.ErrorMessage,
		AcceptedAt: acceptedAt, RejectedAt: rejectedAt,
		UpdatedAt: proposalTimestamp(now), ID: proposal.ID, ProjectID: string(proposal.ProjectID),
		Status_2: string(expected),
	})
	return rows == 1, err
}

// AcceptTaskPlanProposal creates a task plan and marks its proposal accepted in
// one SQLite transaction. A repeated accept returns the same existing plan.
func (s *Store) AcceptTaskPlanProposal(ctx context.Context, projectID domain.ProjectID, proposalID string, plan domain.TaskPlan, now time.Time) (domain.TaskPlanSummary, error) {
	if err := plan.Validate(); err != nil {
		return domain.TaskPlanSummary{}, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.TaskPlanSummary{}, err
	}
	defer s.writeMu.Unlock()
	var summary domain.TaskPlanSummary
	err := s.inTx(ctx, "accept task plan proposal", func(q *gen.Queries) error {
		proposal, err := q.GetTaskPlanProposal(ctx, gen.GetTaskPlanProposalParams{ProjectID: string(projectID), ID: proposalID})
		if err != nil {
			return err
		}
		if proposal.Status == string(domain.TaskPlanProposalAccepted) {
			row, err := q.GetTaskPlan(ctx, gen.GetTaskPlanParams{ProjectID: projectID, ID: plan.ID})
			if err != nil {
				return err
			}
			summary = domain.TaskPlanSummary{ID: row.ID, ProjectID: string(row.ProjectID), Title: row.Title, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
			return nil
		}
		if proposal.Status != string(domain.TaskPlanProposalReady) {
			return fmt.Errorf("%w: %s", domain.ErrTaskPlanProposalNotReady, proposal.Status)
		}
		var stored domain.TaskPlan
		if err := json.Unmarshal([]byte(proposal.GraphJson), &stored); err != nil {
			return fmt.Errorf("decode proposal graph: %w", err)
		}
		if stored.ID != plan.ID || stored.ProjectID != plan.ProjectID {
			return errors.New("proposal graph identity mismatch")
		}
		if err := s.insertTaskPlan(ctx, q, stored, now.UTC()); err != nil {
			return err
		}
		updated, err := q.AcceptTaskPlanProposal(ctx, gen.AcceptTaskPlanProposalParams{
			AcceptedAt: sql.NullString{String: proposalTimestamp(now), Valid: true},
			UpdatedAt:  proposalTimestamp(now), ID: proposalID, ProjectID: string(projectID),
		})
		if err != nil {
			return err
		}
		if updated != 1 {
			return errors.New("task plan proposal acceptance lost its state race")
		}
		summary = domain.TaskPlanSummary{ID: stored.ID, ProjectID: stored.ProjectID, Title: stored.Title, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
		return nil
	})
	if err != nil {
		if isSQLiteUnique(err) || isSQLitePrimaryKey(err) {
			return domain.TaskPlanSummary{}, fmt.Errorf("accept task plan proposal: %w", domain.ErrDuplicateTaskPlan)
		}
		return domain.TaskPlanSummary{}, err
	}
	return summary, nil
}

func (s *Store) RejectTaskPlanProposal(ctx context.Context, projectID domain.ProjectID, id string, now time.Time) (bool, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	rows, err := s.qw.RejectTaskPlanProposal(ctx, gen.RejectTaskPlanProposalParams{
		RejectedAt: sql.NullString{String: proposalTimestamp(now), Valid: true},
		UpdatedAt:  proposalTimestamp(now), ID: id, ProjectID: string(projectID),
	})
	return rows == 1, err
}

func taskPlanProposalFromRow(row gen.TaskPlanProposal) (domain.TaskPlanProposal, error) {
	created, err := time.Parse(proposalTimestampLayout, row.CreatedAt)
	if err != nil {
		return domain.TaskPlanProposal{}, fmt.Errorf("parse task plan proposal created_at: %w", err)
	}
	updated, err := time.Parse(proposalTimestampLayout, row.UpdatedAt)
	if err != nil {
		return domain.TaskPlanProposal{}, fmt.Errorf("parse task plan proposal updated_at: %w", err)
	}
	out := domain.TaskPlanProposal{
		ID: row.ID, ProjectID: domain.ProjectID(row.ProjectID), RequestKey: row.RequestKey,
		Specification: row.Specification, State: domain.TaskPlanProposalState(row.Status),
		OrchestratorID: domain.SessionID(row.OrchestratorID), TurnID: row.TurnID,
		GraphJSON: row.GraphJson, ErrorCode: row.ErrorCode, ErrorMessage: row.ErrorMessage,
		CreatedAt: created, UpdatedAt: updated,
	}
	if row.AcceptedAt.Valid {
		v, err := time.Parse(proposalTimestampLayout, row.AcceptedAt.String)
		if err != nil {
			return domain.TaskPlanProposal{}, err
		}
		out.AcceptedAt = &v
	}
	if row.RejectedAt.Valid {
		v, err := time.Parse(proposalTimestampLayout, row.RejectedAt.String)
		if err != nil {
			return domain.TaskPlanProposal{}, err
		}
		out.RejectedAt = &v
	}
	return out, nil
}
