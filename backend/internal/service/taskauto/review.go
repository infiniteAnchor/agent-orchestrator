package taskauto

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

const reviewTimeout = 15 * time.Minute

// OrchestratorSpawner is the project Chat orchestrator owner.
type OrchestratorSpawner interface {
	SpawnOrchestrator(context.Context, domain.ProjectID, bool, domain.SessionMode) (domain.Session, error)
}

// ReviewChat is the durable Chat surface a reviewer turn uses.
type ReviewChat interface {
	Send(context.Context, domain.SessionID, ports.ChatUserMessage) (domain.ConversationTurn, error)
	Snapshot(context.Context, domain.SessionID) (chatsvc.Snapshot, error)
}

// ChatReviewer asks the project orchestrator for one handoff summary.
// The client message id is the follow-up id, so a restart does not send a
// second turn.
type ChatReviewer struct {
	spawner OrchestratorSpawner
	chat    ReviewChat
}

// NewChatReviewer builds the Chat-backed reviewer.
func NewChatReviewer(spawner OrchestratorSpawner, chat ReviewChat) *ChatReviewer {
	if spawner == nil || chat == nil {
		return nil
	}
	return &ChatReviewer{spawner: spawner, chat: chat}
}

// Review sends or resumes one reviewer turn and returns its assistant text.
func (r *ChatReviewer) Review(ctx context.Context, followup domain.PlannerFollowup) (string, error) {
	if r == nil || r.spawner == nil || r.chat == nil {
		return "", errors.New("reviewer is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()
	session, err := r.spawner.SpawnOrchestrator(ctx, followup.ProjectID, false, domain.SessionModeChat)
	if err != nil {
		return "", err
	}
	if session.Mode != domain.SessionModeChat {
		return "", errors.New("project orchestrator is not in chat mode")
	}
	clientID := "task-followup/" + followup.ID
	turnID := followup.TurnID
	if turnID == "" {
		snapshot, err := r.chat.Snapshot(ctx, session.ID)
		if err != nil {
			return "", err
		}
		turnID = reviewTurnID(snapshot, clientID)
	}
	if turnID == "" {
		turn, err := r.chat.Send(ctx, session.ID, ports.ChatUserMessage{
			Text: reviewPrompt(followup), ClientMessageID: clientID, Origin: domain.MessageOriginAutomation,
		})
		if err != nil {
			return "", err
		}
		turnID = turn.ID
		if turnID == "" {
			snapshot, err := r.chat.Snapshot(ctx, session.ID)
			if err != nil {
				return "", err
			}
			turnID = reviewTurnID(snapshot, clientID)
		}
	}
	if turnID == "" {
		return "", errors.New("reviewer turn was not recorded")
	}
	return waitForReview(ctx, r.chat, session.ID, turnID)
}

func reviewPrompt(followup domain.PlannerFollowup) string {
	return fmt.Sprintf(
		"Review durable task %s attempt %s in plan %s. The task result is already stored. Reply with a handoff summary a person or the next worker can use. Do not include host paths, credentials, or a request to dispatch work. Keep the summary under 2000 characters.",
		followup.TaskID, followup.AttemptID, followup.PlanID,
	)
}

func reviewTurnID(snapshot chatsvc.Snapshot, clientMessageID string) string {
	for _, message := range snapshot.Messages {
		if message.ClientMessageID == clientMessageID && message.TurnID != "" {
			return message.TurnID
		}
	}
	return ""
}

func waitForReview(ctx context.Context, chat ReviewChat, session domain.SessionID, turnID string) (string, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := chat.Snapshot(ctx, session)
		if err != nil {
			return "", err
		}
		for _, turn := range snapshot.Turns {
			if turn.ID != turnID || !turn.State.Terminal() {
				continue
			}
			if turn.State != domain.TurnStateCompleted {
				return "", errors.New("reviewer turn did not complete")
			}
			for i := len(snapshot.Messages) - 1; i >= 0; i-- {
				message := snapshot.Messages[i]
				if message.TurnID == turnID && message.Role == domain.MessageRoleAssistant && !message.Streaming {
					return strings.TrimSpace(message.Text), nil
				}
			}
			return "", errors.New("reviewer turn has no assistant result")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}
