package bridge_test

import (
	"context"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridgetest"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

var (
	dmKey    = bridgetest.DMKey
	groupKey = bridgetest.GroupKey
)

func TestIncomingMessageCreatesConversation(t *testing.T) {
	h := bridgetest.New(t)
	h.Queue(bridgetest.TextMessage(dmKey(), "alice", "m1", "hello there", time.UnixMilli(1_700_000_000_000)))

	list, err := h.Store.ListConversations(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Kind != "dm" || list[0].Name != "Alice" || list[0].Preview != "hello there" || list[0].UnreadCount != 1 {
		t.Fatalf("unexpected conversation list: %+v", list)
	}
	msgs := h.Messages(dmKey())
	if len(msgs) != 1 || msgs[0].Body != "hello there" || msgs[0].AuthorName != "Alice" || msgs[0].Outgoing || msgs[0].SentAt != 1_700_000_000_000 {
		t.Fatalf("unexpected messages: %+v", msgs)
	}

	// The same message again (eg redelivered after a reconnect) isn't duplicated.
	h.Queue(bridgetest.TextMessage(dmKey(), "alice", "m1", "hello there", time.UnixMilli(1_700_000_000_000)))
	if msgs = h.Messages(dmKey()); len(msgs) != 1 {
		t.Fatalf("message duplicated: %+v", msgs)
	}
}

func TestOwnMessageFromOtherDeviceIsOutgoing(t *testing.T) {
	h := bridgetest.New(t)
	h.Queue(bridgetest.TextMessage(dmKey(), "alice", "m1", "hi", time.UnixMilli(1_700_000_000_000)))
	h.Queue(bridgetest.TextMessage(dmKey(), "me", "m2", "hi back", time.UnixMilli(1_700_000_001_000)))
	msgs := h.Messages(dmKey())
	if len(msgs) != 2 || !msgs[1].Outgoing || msgs[1].AuthorName != "Me Myself" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
}

func TestReactionEditReceiptAndDelete(t *testing.T) {
	h := bridgetest.New(t)
	h.Queue(bridgetest.TextMessage(dmKey(), "me", "m1", "original", time.UnixMilli(1_700_000_000_000)))

	h.Queue(&simplevent.Reaction{
		EventMeta:     simplevent.EventMeta{Type: bridgev2.RemoteEventReaction, PortalKey: dmKey(), Sender: bridgetest.Sender("alice")},
		TargetMessage: "m1",
		Emoji:         "👍",
	})
	edit := bridgetest.TextMessage(dmKey(), "me", "", "", time.UnixMilli(1_700_000_005_000))
	edit.Type = bridgev2.RemoteEventEdit
	edit.TargetMessage = "m1"
	edit.ConvertEditFunc = func(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, existing []*database.Message, _ string) (*bridgev2.ConvertedEdit, error) {
		return &bridgev2.ConvertedEdit{ModifiedParts: []*bridgev2.ConvertedEditPart{{
			Part: existing[0], Type: event.EventMessage, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "edited"},
		}}}, nil
	}
	h.Queue(edit)
	h.Queue(&simplevent.Receipt{
		EventMeta:  simplevent.EventMeta{Type: bridgev2.RemoteEventReadReceipt, PortalKey: dmKey(), Sender: bridgetest.Sender("alice")},
		LastTarget: "m1",
	})

	msgs := h.Messages(dmKey())
	if len(msgs) != 1 {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	m := msgs[0]
	if m.Body != "edited" || m.EditedAt != 1_700_000_005_000 {
		t.Fatalf("edit not applied: %+v", m)
	}
	if len(m.Reactions) != 1 || m.Reactions[0].Emoji != "👍" || m.Reactions[0].ReactorName != "Alice" {
		t.Fatalf("reaction not applied: %+v", m.Reactions)
	}
	if m.Status != "" {
		t.Fatalf("message from another device shouldn't have a send status: %q", m.Status)
	}

	h.Queue(&simplevent.MessageRemove{
		EventMeta:     simplevent.EventMeta{Type: bridgev2.RemoteEventMessageRemove, PortalKey: dmKey(), Sender: bridgetest.Sender("me")},
		TargetMessage: "m1",
	})
	m = h.Messages(dmKey())[0]
	if !m.Deleted || m.Body != "" || len(m.Reactions) != 0 {
		t.Fatalf("delete not applied: %+v", m)
	}
}

func TestGroupMembershipAndBackfill(t *testing.T) {
	h := bridgetest.New(t)
	old1 := bridgetest.BackfillText("bob", "old1", "old one", time.UnixMilli(1_600_000_000_000))
	old1.Reactions = []*bridgev2.BackfillReaction{{Sender: bridgetest.Sender("alice"), Emoji: "😂"}}
	h.Net.SetHistory(groupKey().ID, []*bridgev2.BackfillMessage{
		old1, bridgetest.BackfillText("me", "old2", "old two", time.UnixMilli(1_600_000_001_000)),
	})
	h.Queue(&simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventChatResync, PortalKey: groupKey(), CreatePortal: true},
	})

	detail, err := h.Store.ConversationDetail(h.Ctx, h.ConversationID(groupKey()))
	if err != nil {
		t.Fatal(err)
	}
	if detail.Summary.Name != "Weekend plans" || detail.Summary.Kind != "group" || len(detail.Members) != 3 {
		t.Fatalf("unexpected group: %+v", detail)
	}
	msgs := h.Messages(groupKey())
	if len(msgs) != 2 || msgs[0].Body != "old one" || msgs[1].Body != "old two" || !msgs[1].Outgoing {
		t.Fatalf("backfill not stored: %+v", msgs)
	}
	if len(msgs[0].Reactions) != 1 || msgs[0].Reactions[0].Emoji != "😂" {
		t.Fatalf("backfilled reaction not stored: %+v", msgs[0].Reactions)
	}

	// Bob leaving shows up in the timeline.
	h.Queue(&simplevent.ChatInfoChange{
		EventMeta: simplevent.EventMeta{
			Type: bridgev2.RemoteEventChatInfoChange, PortalKey: groupKey(),
			Sender: bridgetest.Sender("bob"), Timestamp: time.UnixMilli(1_700_000_000_000),
		},
		ChatInfoChange: &bridgev2.ChatInfoChange{MemberChanges: &bridgev2.ChatMemberList{MemberMap: bridgev2.ChatMemberMap{
			"bob": {EventSender: bridgetest.Sender("bob"), Membership: event.MembershipLeave},
		}}},
	})
	msgs = h.Messages(groupKey())
	last := msgs[len(msgs)-1]
	if last.Kind != "update" || last.AuthorName != "Bob" || last.Body != "left the group" {
		t.Fatalf("leave not in timeline: %+v", last)
	}
}

func TestSendFromHereGetsStatus(t *testing.T) {
	h := bridgetest.New(t)
	h.Queue(bridgetest.TextMessage(dmKey(), "alice", "m1", "hi", time.UnixMilli(1_700_000_000_000)))
	conversationID := h.ConversationID(dmKey())

	if _, err := h.Svc.Send(h.Ctx, conversationID, "sent from here", 0, nil); err != nil {
		t.Fatal(err)
	}
	last := h.WaitFor(dmKey(), func(m store.Message) bool { return m.Status == "sent" })
	if last.Body != "sent from here" || !last.Outgoing {
		t.Fatalf("unexpected message: %+v", last)
	}
	sent := h.Net.Sent()
	if len(sent) != 1 || sent[0].Content.Body != "sent from here" {
		t.Fatalf("message not handed to the network: %+v", sent)
	}

	// Alice reading it marks it read.
	h.Queue(&simplevent.Receipt{
		EventMeta:  simplevent.EventMeta{Type: bridgev2.RemoteEventReadReceipt, PortalKey: dmKey(), Sender: bridgetest.Sender("alice")},
		LastTarget: "sent-1",
	})
	msgs := h.Messages(dmKey())
	if last = msgs[len(msgs)-1]; last.Status != "read" {
		t.Fatalf("expected read status, got %+v", last)
	}
}

func TestFailedSendRetryAndDiscard(t *testing.T) {
	h := bridgetest.New(t)
	h.Queue(bridgetest.TextMessage(dmKey(), "alice", "m1", "hi", time.UnixMilli(1_700_000_000_000)))
	conversationID := h.ConversationID(dmKey())

	h.Net.SetFailSend(true)
	failedID, err := h.Svc.Send(h.Ctx, conversationID, "first try", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed := h.WaitFor(dmKey(), func(m store.Message) bool { return m.Status == "failed" })
	if failed.ID != failedID || failed.Body != "first try" || failed.Error == "" {
		t.Fatalf("unexpected failed message: %+v", failed)
	}

	h.Net.SetFailSend(false)
	if _, err = h.Svc.Retry(h.Ctx, failedID); err != nil {
		t.Fatal(err)
	}
	sent := h.WaitFor(dmKey(), func(m store.Message) bool { return m.Status == "sent" })
	if sent.Body != "first try" || sent.ID == failedID {
		t.Fatalf("unexpected retried message: %+v", sent)
	}
	for _, m := range h.Messages(dmKey()) {
		if m.ID == failedID {
			t.Fatalf("failed copy wasn't removed")
		}
	}
	if err = h.Svc.Discard(h.Ctx, sent.ID); err == nil {
		t.Fatalf("discarding a sent message should fail")
	}
}

func TestImageMessage(t *testing.T) {
	h := bridgetest.New(t)
	png := []byte("\x89PNG\r\n\x1a\nnot really a png")
	h.Queue(bridgetest.ImageMessage("alice", "img-1", png, "look at this", time.UnixMilli(1_700_000_000_000)))
	msgs := h.Messages(dmKey())
	if len(msgs) != 1 || len(msgs[0].Attachments) != 1 {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	a := msgs[0].Attachments[0]
	if a.Status != "present" || a.Caption != "look at this" || a.Filename != "photo.png" || a.ContentType != "image/png" {
		t.Fatalf("unexpected attachment: %+v", a)
	}
	f, err := h.Store.AttachmentFile(h.Ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.SHA256 == "" || localmx.SHAFromContentURI(localmx.ContentURI(f.SHA256)) != f.SHA256 {
		t.Fatalf("unexpected file: %+v", f)
	}
}
