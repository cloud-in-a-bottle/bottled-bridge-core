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

// Publish never blocks: a subscriber that has fallen far behind misses events, and resyncs when it reconnects.
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
		}
	}
}

func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subscribers, ch)
		b.mu.Unlock()
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
