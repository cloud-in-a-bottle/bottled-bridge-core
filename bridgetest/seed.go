package bridgetest

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

// SeedDevData fills dir, laid out like an app's data, with sample chats for looking at the UI. dbOwner is the app's
// (bridge.Options.DBOwner), and png is a picture to send (eg the app's icon). Call it from an app's test, skipped
// unless asked for:
//
//	DEV_SEED_DIR=$PWD/dev-data go test -tags sqlite_fts5 -run TestSeedDevData ./...
func SeedDevData(t *testing.T, dir, dbOwner string, png []byte) {
	h := NewIn(t, dir, Options{DBOwner: dbOwner})
	base := time.Now().Add(-26 * time.Hour)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	h.Queue(TextMessage(DMKey(), "alice", "a1", "Hey! Are we still on for Saturday?", at(0)))
	h.Queue(TextMessage(DMKey(), "me", "a2", "Yes! Looking forward to it", at(3)))
	h.Queue(ImageMessage("alice", "img-a3", png, "Found this bottle at the market 😄", at(5)))
	h.Queue(&simplevent.Reaction{
		EventMeta:     simplevent.EventMeta{Type: bridgev2.RemoteEventReaction, PortalKey: DMKey(), Sender: Sender("me")},
		TargetMessage: "img-a3",
		Emoji:         "😆",
	})
	h.Queue(TextMessage(DMKey(), "alice", "a4", "Also https://example.com has the details", at(24*60+30)))
	h.Net.SetHistory(GroupKey().ID, []*bridgev2.BackfillMessage{BackfillText("bob", "g1", "Who's bringing snacks?", at(60))})
	h.Queue(&simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: GroupKey(), CreatePortal: true},
	})
	h.Queue(TextMessage(GroupKey(), "alice", "g2", "I can bring chips", at(62)))
}
