package bridge_test

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridgetest"
)

func TestLoadOlderFetchesAPageAtATime(t *testing.T) {
	h := bridgetest.New(t)
	sub, unsubscribe := h.Broker.Subscribe()
	defer unsubscribe()
	at := func(s int64) time.Time { return time.Unix(1_700_000_000+s, 0) }
	h.Net.SetOlderHistory(dmKey().ID,
		[]*bridgev2.BackfillMessage{bridgetest.BackfillText("alice", "o3", "older three", at(-10)), bridgetest.BackfillText("me", "o4", "older four", at(-5))},
		[]*bridgev2.BackfillMessage{bridgetest.BackfillText("alice", "o1", "older one", at(-30))},
	)
	h.Queue(bridgetest.TextMessage(dmKey(), "alice", "m1", "latest", at(0)))
	conversationID := h.ConversationID(dmKey())

	state, err := h.Svc.History(h.Ctx, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasMore {
		t.Fatalf("expected older history to be available: %+v", state)
	}
	waitHistory := func() bridge.HistoryState {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case evt := <-sub:
				if evt.Kind == "history" && !strings.Contains(string(evt.Data), `"loading":true`) {
					state, err := h.Svc.History(h.Ctx, conversationID)
					if err != nil {
						t.Fatal(err)
					}
					return state
				}
			case <-timeout:
				t.Fatal("timed out waiting for history")
			}
		}
	}

	if err = h.Svc.LoadOlder(h.Ctx, conversationID); err != nil {
		t.Fatal(err)
	}
	if state = waitHistory(); !state.HasMore {
		t.Fatalf("expected more after the first page: %+v", state)
	}
	msgs := h.Messages(dmKey())
	if len(msgs) != 3 || msgs[0].Body != "older three" || !msgs[1].Outgoing || msgs[2].Body != "latest" {
		t.Fatalf("unexpected messages after one page: %+v", msgs)
	}

	if err = h.Svc.LoadOlder(h.Ctx, conversationID); err != nil {
		t.Fatal(err)
	}
	if state = waitHistory(); state.HasMore {
		t.Fatalf("expected the end of history: %+v", state)
	}
	if msgs = h.Messages(dmKey()); len(msgs) != 4 || msgs[0].Body != "older one" {
		t.Fatalf("unexpected messages after two pages: %+v", msgs)
	}
}
