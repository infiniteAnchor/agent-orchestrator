package domain

import (
	"errors"
	"time"
)

// Task plan proposal errors describe missing projects and proposals awaiting review.
var (
	ErrTaskPlanProposalNotReady        = errors.New("domain: task plan proposal is not ready")
	ErrTaskPlanProposalProjectNotFound = errors.New("domain: active task plan proposal project not found")
)

// TaskPlanProposalState is the durable review lifecycle of a planner result.
// Proposals are intentionally separate from task plans until explicit approval.
type TaskPlanProposalState string

// Task plan proposal lifecycle states.
const (
	TaskPlanProposalQueued     TaskPlanProposalState = "queued"
	TaskPlanProposalGenerating TaskPlanProposalState = "generating"
	TaskPlanProposalReady      TaskPlanProposalState = "ready"
	TaskPlanProposalInvalid    TaskPlanProposalState = "invalid"
	TaskPlanProposalFailed     TaskPlanProposalState = "failed"
	TaskPlanProposalAccepted   TaskPlanProposalState = "accepted"
	TaskPlanProposalRejected   TaskPlanProposalState = "rejected"
)

// TaskPlanProposal stores a bounded specification and a candidate graph until
// a user accepts it. GraphJSON is internal durable state, not a provider DTO.
type TaskPlanProposal struct {
	ID             string
	ProjectID      ProjectID
	RequestKey     string
	Specification  string
	State          TaskPlanProposalState
	OrchestratorID SessionID
	TurnID         string
	GraphJSON      string
	ErrorCode      string
	ErrorMessage   string
	AcceptedAt     *time.Time
	RejectedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
