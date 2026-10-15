package bridge

import (
	"context"
	"errors"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// ErrHistoryBusy is returned while older history for the conversation is already being fetched.
var ErrHistoryBusy = errors.New("already loading earlier messages for this chat")

// HistoryState says whether a conversation has older history on the network that isn't here yet.
type HistoryState struct {
	ConversationID int64 `json:"conversation_id"`
	// The network may have older messages, and LoadOlder can fetch them.
	HasMore bool `json:"has_more"`
	Loading bool `json:"loading"`
	// Why the last fetch failed, if it did.
	Error string `json:"error,omitempty"`
}

func (s *Service) portalFor(ctx context.Context, conversationID int64) (*bridgev2.Portal, error) {
	target, err := s.store.ConversationTarget(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	portal, err := s.br.GetPortalByMXID(ctx, id.RoomID(target.RoomID))
	if err != nil {
		return nil, err
	} else if portal == nil {
		return nil, store.ErrNotFound
	}
	return portal, nil
}

// History reports whether LoadOlder could fetch more for a conversation.
func (s *Service) History(ctx context.Context, conversationID int64) (HistoryState, error) {
	state := HistoryState{ConversationID: conversationID}
	s.historyMu.Lock()
	state.Loading = s.historyBusy[conversationID]
	s.historyMu.Unlock()
	portal, err := s.portalFor(ctx, conversationID)
	if errors.Is(err, store.ErrReadOnly) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	task, err := s.br.DB.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, true)
	if err != nil {
		return state, err
	}
	state.HasMore = task != nil && !task.IsDone
	return state, nil
}

// LoadOlder fetches one page (the backfill queue's batch size) of older messages for a conversation from the
// network, in the background, as scrolling up in a normal client would. It publishes a "history" event with the new
// HistoryState when done.
func (s *Service) LoadOlder(ctx context.Context, conversationID int64) error {
	if s.CurrentLogin(ctx) == nil {
		return ErrNotLoggedIn
	}
	portal, err := s.portalFor(ctx, conversationID)
	if err != nil {
		return err
	}
	task, err := s.br.DB.BackfillTask.GetNextForPortal(ctx, portal.PortalKey, false)
	if err != nil {
		return err
	} else if task == nil {
		s.broker.Publish("history", HistoryState{ConversationID: conversationID})
		return nil
	}
	s.historyMu.Lock()
	if s.historyBusy[conversationID] {
		s.historyMu.Unlock()
		return ErrHistoryBusy
	}
	s.historyBusy[conversationID] = true
	s.historyMu.Unlock()
	s.broker.Publish("history", HistoryState{ConversationID: conversationID, HasMore: true, Loading: true})
	go func() {
		bgCtx := s.log.With().Int64("conversation_id", conversationID).Logger().WithContext(s.br.BackgroundCtx)
		// The bridge's own backfill-queue step: it sends the batch to the store (via BatchSend) and moves the task's
		// cursor on, so the next call continues from there. On failure it logs, and waits a minute before returning.
		s.br.DoBackfillTask(bgCtx, task)
		s.historyMu.Lock()
		delete(s.historyBusy, conversationID)
		s.historyMu.Unlock()
		state, err := s.History(bgCtx, conversationID)
		if err != nil {
			state.Error = err.Error()
		}
		s.broker.Publish("history", state)
		s.broker.Publish("conversation", events.ConversationChanged{ConversationID: conversationID})
	}()
	return nil
}
