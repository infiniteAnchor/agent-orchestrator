package session

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TaskWorkerAlive observes a task worker without restoring or starting it.
// Managers without this capability cannot prove a launch; callers must hold.
func (s *Service) TaskWorkerAlive(ctx context.Context, id domain.SessionID) (bool, error) {
	probe, ok := s.manager.(interface {
		TaskWorkerAlive(context.Context, domain.SessionID) (bool, error)
	})
	if !ok {
		return false, fmt.Errorf("task worker observation unavailable")
	}
	return probe.TaskWorkerAlive(ctx, id)
}
