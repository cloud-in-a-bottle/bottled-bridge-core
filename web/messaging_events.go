package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cloud-in-a-bottle/messaging-service-spec/messaging"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
)

type emptyEvent struct{}

// svcEvents streams the service's events, translated from the UI's.
func (s *Server) svcEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		svcError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := s.broker.Subscribe()
	defer unsubscribe()
	if err := messaging.WriteEvent(w, messaging.EventHello, emptyEvent{}); err != nil {
		return
	}
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
		case evt, open := <-ch:
			// Dropped for falling behind: end the stream, so the client reconnects and resyncs on "hello".
			if !open {
				return
			}
			name, data, ok := svcEvent(evt)
			if !ok {
				continue
			}
			if err := messaging.WriteEvent(w, name, data); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}

func svcEvent(evt events.Event) (string, any, bool) {
	switch evt.Kind {
	case "conversation":
		var c events.ConversationChanged
		if json.Unmarshal(evt.Data, &c) != nil {
			return "", nil, false
		}
		return messaging.EventChat, messaging.ChatChanged{ChatID: svcID(c.ConversationID)}, true
	case "history":
		var h bridge.HistoryState
		if json.Unmarshal(evt.Data, &h) != nil {
			return "", nil, false
		}
		return messaging.EventChat, messaging.ChatChanged{ChatID: svcID(h.ConversationID)}, true
	case "typing":
		var t events.Typing
		if json.Unmarshal(evt.Data, &t) != nil {
			return "", nil, false
		}
		return messaging.EventTyping, messaging.TypingChanged{
			ChatID: svcID(t.ConversationID), PersonID: svcID(t.PersonID), Typing: t.Started,
		}, true
	case "status", "account":
		return messaging.EventAccount, emptyEvent{}, true
	}
	return "", nil, false
}
