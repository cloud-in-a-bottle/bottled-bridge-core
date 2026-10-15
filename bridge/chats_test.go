package bridge_test

import (
	"slices"
	"testing"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridgetest"
)

func TestStartChats(t *testing.T) {
	h := bridgetest.New(t)
	// A known user goes straight to the connector's chat creation; something typed is resolved first.
	byPerson, err := h.Svc.StartChatWithPerson(h.Ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	byPhone, err := h.Svc.StartChat(h.Ctx, "+1")
	if err != nil {
		t.Fatal(err)
	}
	if byPerson != byPhone || byPerson != h.ConversationID(bridgetest.DMKey()) {
		t.Fatalf("expected both to open the DM with alice: %d, %d", byPerson, byPhone)
	}
	if started := h.Net.ChatsStarted(); !slices.Equal(started, []string{"ghost:alice", "resolve:+1"}) {
		t.Fatalf("unexpected chat starts: %v", started)
	}
	if _, err = h.Svc.StartChat(h.Ctx, "nobody"); err == nil {
		t.Fatal("expected an error for an unknown identifier")
	}
}
