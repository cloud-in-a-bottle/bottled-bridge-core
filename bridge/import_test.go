package bridge_test

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridgetest"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

func importedText(sender networkid.UserID, id networkid.MessageID, body string, ts time.Time) *bridge.ImportedMessage {
	return &bridge.ImportedMessage{
		ConvertedMessage: &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
			Type: event.EventMessage, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: body},
		}}},
		ID: id, Sender: bridgetest.Sender(sender), Timestamp: ts,
	}
}

func TestImportMessages(t *testing.T) {
	h := bridgetest.New(t)
	base := time.UnixMilli(1_700_000_000_000)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }

	if _, err := h.Svc.ImportChat(h.Ctx, dmKey(), nil); err != nil {
		t.Fatal(err)
	}
	read := importedText("alice", "i1", "hi from the backup", at(0))
	read.Read = true
	mine := importedText("me", "i2", "my reply", at(10))
	mine.Status = "delivered"
	mine.ReadBy = []bridge.ImportedReceipt{{Sender: "alice", At: at(20)}}
	edited := importedText("alice", "i3-v2", "second version", at(40))
	edited.OtherIDs = []networkid.MessageID{"i3"}
	edited.Revisions = []bridge.ImportedRevision{{Timestamp: at(30), Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "first version"}}}
	deleted := importedText("me", "i4", "oops", at(50))
	deleted.DeletedAt = at(55)
	reacted := importedText("alice", "i5", "react to this", at(60))
	reacted.Reactions = []*bridgev2.BackfillReaction{{Sender: bridgetest.Sender("me"), Emoji: "👍", Timestamp: at(61)}}
	reply := importedText("me", "i6", "replying", at(70))
	reply.ReplyTo = &networkid.MessageOptionalPartID{MessageID: "i1"}

	counts, err := h.Svc.ImportMessages(h.Ctx, dmKey(), []*bridge.ImportedMessage{read, mine, edited, deleted, reacted, reply})
	if err != nil {
		t.Fatal(err)
	}
	if counts != (bridge.ImportCounts{Imported: 6}) {
		t.Fatalf("counts: %+v", counts)
	}
	msgs := h.Messages(dmKey())
	if len(msgs) != 6 {
		t.Fatalf("messages: %+v", msgs)
	}
	if msgs[0].Body != "hi from the backup" || msgs[0].SentAt != at(0).UnixMilli() || msgs[0].Outgoing {
		t.Fatalf("first message: %+v", msgs[0])
	}
	if !msgs[1].Outgoing || msgs[1].Status != "read" {
		t.Fatalf("own message should show as read by alice: %+v", msgs[1])
	}
	if msgs[2].Body != "second version" || msgs[2].EditedAt != at(40).UnixMilli() || msgs[2].SentAt != at(30).UnixMilli() {
		t.Fatalf("edited message: %+v", msgs[2])
	}
	if !msgs[3].Deleted || msgs[3].Body != "" {
		t.Fatalf("deleted message: %+v", msgs[3])
	}
	if len(msgs[4].Reactions) != 1 || msgs[4].Reactions[0].Emoji != "👍" || !msgs[4].Reactions[0].Mine {
		t.Fatalf("reactions: %+v", msgs[4].Reactions)
	}
	if msgs[5].Quote == nil || msgs[5].Quote.MessageID != msgs[0].ID {
		t.Fatalf("reply: %+v", msgs[5])
	}
	list, err := h.Store.ListConversations(h.Ctx)
	if err != nil || len(list) != 1 || list[0].UnreadCount != 2 {
		// i3 and i5 weren't marked read in the export.
		t.Fatalf("conversations: %+v, %v", list, err)
	}

	// The same messages again, from the network or another import, aren't duplicated, and the bridge knows the
	// imported ones: a reaction to one arriving live lands on it.
	h.Queue(bridgetest.TextMessage(dmKey(), "alice", "i1", "hi from the backup", at(0)))
	// A later export with the message edited again lists the ID it has here among its earlier ones.
	editedAgain := importedText("alice", "i3-v3", "third version", at(45))
	editedAgain.OtherIDs = []networkid.MessageID{"i3", "i3-v2"}
	again, err := h.Svc.ImportMessages(h.Ctx, dmKey(), []*bridge.ImportedMessage{editedAgain, read})
	if err != nil || again != (bridge.ImportCounts{Skipped: 2}) {
		t.Fatalf("reimport: %+v, %v", again, err)
	}
	h.Queue(&simplevent.Reaction{
		EventMeta:     simplevent.EventMeta{Type: bridgev2.RemoteEventReaction, PortalKey: dmKey(), Sender: bridgetest.Sender("alice")},
		TargetMessage: "i2",
		Emoji:         "❤️",
	})
	msgs = h.Messages(dmKey())
	if len(msgs) != 6 || len(msgs[1].Reactions) != 1 || msgs[1].Reactions[0].Emoji != "❤" {
		t.Fatalf("after live events: %+v", msgs)
	}

	// What an app writing a backup reads back.
	var exported []store.ExportMessage
	if err = h.Store.ExportMessages(h.Ctx, func(m store.ExportMessage) error {
		exported = append(exported, m)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(exported) != 6 {
		t.Fatalf("exported %d messages", len(exported))
	}
	if e := exported[2]; e.RemoteID != "i3-v2" || e.Body != "second version" || len(e.Edits) != 1 ||
		e.Edits[0] != (store.ExportEdit{Body: "first version", EditedAt: at(40).UnixMilli()}) {
		t.Fatalf("exported edit: %+v", e)
	}
	if e := exported[3]; e.DeletedAt != at(55).UnixMilli() || e.Body != "" {
		t.Fatalf("exported deletion: %+v", e)
	}
	if e := exported[5]; e.ReplyTo == nil || e.ReplyTo.RemoteID != "i1" || e.ReplyTo.AuthorRemoteID != "alice" {
		t.Fatalf("exported reply: %+v", e)
	}
	if e := exported[1]; e.Status != "delivered" || len(e.Reactions) != 1 || e.Reactions[0].ReactorRemoteID != "alice" {
		t.Fatalf("exported own message: %+v", e)
	}
	conversations, err := h.Store.ExportConversations(h.Ctx)
	if err != nil || len(conversations) != 1 {
		t.Fatalf("exported conversations: %+v, %v", conversations, err)
	}
	if c := conversations[0]; c.ThreadKey != "alice" || c.Kind != store.KindDM || c.LastReadAt != at(0).UnixMilli() ||
		len(c.ReadReceipts) != 1 || c.ReadReceipts[0].RemoteID != "alice" || c.ReadReceipts[0].ReadUpTo != at(10).UnixMilli() {
		t.Fatalf("exported conversation: %+v", c)
	}
}

func TestImportMessagesNeedsTheChat(t *testing.T) {
	h := bridgetest.New(t)
	_, err := h.Svc.ImportMessages(h.Ctx, groupKey(), []*bridge.ImportedMessage{importedText("bob", "x", "hi", time.Now())})
	if err == nil {
		t.Fatal("import into a chat that doesn't exist succeeded")
	}
}
