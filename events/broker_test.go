package events

import "testing"

func TestSlowSubscriberIsDropped(t *testing.T) {
	b := NewBroker()
	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()
	for range 300 {
		b.Publish("conversation", ConversationChanged{ConversationID: 1})
	}
	n := 0
	for range ch {
		n++
	}
	if n != 256 {
		t.Fatalf("expected the buffered events and then a closed channel, got %d events", n)
	}
	// Unsubscribing after being dropped is harmless.
	unsubscribe()
}
