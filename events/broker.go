// Package events fans change notifications out to browsers connected over SSE.
package events

import (
	"encoding/json"
	"sync"
)

// Event is pushed to browsers as an SSE event named Kind, with Data as its JSON payload.
type Event struct {
	Kind string
	Data json.RawMessage
}

type Broker struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
}

func NewBroker() *Broker {
	return &Broker{subscribers: make(map[chan Event]struct{})}
}

// Publish never blocks: a subscriber that has fallen far behind is dropped, its channel closed, so that it reconnects
// and resyncs rather than carrying on without the events it missed.
func (b *Broker) Publish(kind string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers {
		select {
		case ch <- Event{Kind: kind, Data: data}:
		default:
			delete(b.subscribers, ch)
			close(ch)
		}
	}
}

// Subscribe returns a channel of events, closed if the subscriber falls behind, and a function to unsubscribe.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subscribers[ch]; ok {
			delete(b.subscribers, ch)
			close(ch)
		}
	}
}

// ConversationChanged is the payload of "conversation" events.
type ConversationChanged struct {
	ConversationID int64 `json:"conversationId"`
}

// Typing is the payload of "typing" events.
type Typing struct {
	ConversationID int64 `json:"conversationId"`
	PersonID       int64 `json:"personId"`
	Started        bool  `json:"started"`
}
