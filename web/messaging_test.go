package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cloud-in-a-bottle/messaging-service-spec/messaging"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridgetest"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/web"
)

// newProvider serves the app behind a stand-in for the openhost router: service calls are forwarded to the
// provider's endpoint with the given grants, as the router does.
func newProvider(t *testing.T, grants ...string) (*messaging.Provider, *bridgetest.Harness) {
	h := bridgetest.New(t)
	srv, err := web.New(web.Options{
		Service: h.Svc, Store: h.Store, Files: h.Files, Broker: h.Broker,
		Branding: web.Branding{
			Title: "Bottled Fake", Network: "Fake", AccentColor: "#123456", LoginIntro: "intro", HistoryHelp: "help",
			OnDemandHistory: true,
		},
		Static:    fstest.MapFS{"favicon.svg": {Data: []byte("<svg/>")}, "apple-touch-icon.png": {Data: []byte("png")}},
		Messaging: &web.MessagingOptions{Network: "fake"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var perms []map[string]string
	for _, g := range grants {
		perms = append(perms, map[string]string{"grant": g, "scope": "global"})
	}
	permHeader, _ := json.Marshal(perms)
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/api/services/v2/call/messaging/")
		if !ok || r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "bad service call", http.StatusBadRequest)
			return
		}
		r.URL.Path = web.MessagingPrefix + rest
		r.Header.Set("X-OpenHost-Permissions", string(permHeader))
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(router.Close)
	client := &messaging.Client{RouterURL: router.URL, AppToken: "token", Shortname: "messaging", HTTP: router.Client()}
	return client.Provider("fake-app"), h
}

func errCode(err error) string {
	var e *messaging.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestMessagingNeedsGrants(t *testing.T) {
	p, _ := newProvider(t)
	if _, err := p.Account(context.Background()); errCode(err) != messaging.ErrPermissionRequired {
		t.Fatalf("expected permission_required, got %v", err)
	}
	p, _ = newProvider(t, messaging.GrantRead)
	ctx := context.Background()
	if _, err := p.Account(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkRead(ctx, "1", ""); errCode(err) != messaging.ErrPermissionRequired {
		t.Fatalf("expected a write to need the write grant, got %v", err)
	}
}

func TestMessagingReads(t *testing.T) {
	p, h := newProvider(t, messaging.GrantRead)
	ctx := context.Background()
	base := time.UnixMilli(1_700_000_000_000)
	h.Queue(bridgetest.TextMessage(bridgetest.GroupKey(), "bob", "g1", "anyone up for hiking", base))
	h.Queue(bridgetest.TextMessage(bridgetest.DMKey(), "alice", "m1", "hi there", base.Add(time.Minute)))

	account, err := p.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if account.Network != "fake" || account.NetworkName != "Fake" || !account.Self.IsSelf ||
		*account.Self.Address != "fake:me" || !account.Capabilities.Reply || account.Capabilities.Edit == nil ||
		!account.Capabilities.LoadOlder {
		t.Fatalf("unexpected account: %+v", account)
	}

	page, err := p.Chats(ctx, messaging.ChatsQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Chats) != 1 || page.Chats[0].Kind != messaging.ChatDM || page.Chats[0].OtherPerson == nil ||
		*page.Chats[0].OtherPerson.Address != "fake:alice" || page.NextCursor == nil {
		t.Fatalf("unexpected first page: %+v", page)
	}
	page, err = p.Chats(ctx, messaging.ChatsQuery{Limit: 1, Cursor: *page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Chats) != 1 || page.Chats[0].Title != "Weekend plans" || page.NextCursor != nil {
		t.Fatalf("unexpected second page: %+v", page)
	}

	detail, err := p.Chat(ctx, page.Chats[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Participants) != 3 || detail.Capabilities == nil || !detail.Capabilities.Reply {
		t.Fatalf("unexpected detail: %+v", detail)
	}

	recent, err := p.Recent(ctx, messaging.RecentQuery{Since: base.Add(30 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent.Messages) != 1 || *recent.Messages[0].Text != "hi there" {
		t.Fatalf("unexpected recent: %+v", recent)
	}

	hits, err := p.Search(ctx, "hik", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits.Hits) != 1 || hits.Hits[0].ChatTitle != "Weekend plans" || *hits.Hits[0].Message.Sender.Name != "Bob" {
		t.Fatalf("unexpected search: %+v", hits)
	}
}

// Pages must neither skip nor repeat messages that share a timestamp.
func TestMessagingPagesThroughTies(t *testing.T) {
	p, h := newProvider(t, messaging.GrantRead)
	ctx := context.Background()
	at := time.UnixMilli(1_700_000_000_000)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		h.Queue(bridgetest.TextMessage(bridgetest.DMKey(), "alice", networkid.MessageID("m-"+id), id, at))
	}
	chatID := firstChat(t, p)
	var got []string
	page, err := p.Messages(ctx, chatID, messaging.MessagesQuery{Limit: 2})
	for err == nil {
		var texts []string
		for _, m := range page.Messages {
			texts = append(texts, *m.Text)
		}
		got = append(texts, got...)
		if !page.HasMoreBefore {
			break
		}
		page, err = p.Messages(ctx, chatID, messaging.MessagesQuery{Limit: 2, Before: *page.BeforeCursor})
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "") != "abcde" {
		t.Fatalf("paged out %v", got)
	}

	around, err := p.Messages(ctx, chatID, messaging.MessagesQuery{Limit: 3, Around: firstMessageID(t, p, chatID, "c")})
	if err != nil {
		t.Fatal(err)
	}
	if len(around.Messages) != 3 || *around.Messages[1].Text != "c" || !around.HasMoreBefore || !around.HasMoreAfter {
		t.Fatalf("unexpected page around c: %+v", around)
	}
	after, err := p.Messages(ctx, chatID, messaging.MessagesQuery{After: *around.AfterCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != 1 || *after.Messages[0].Text != "e" || after.HasMoreAfter {
		t.Fatalf("unexpected page after: %+v", after)
	}
}

func firstChat(t *testing.T, p *messaging.Provider) string {
	t.Helper()
	page, err := p.Chats(context.Background(), messaging.ChatsQuery{})
	if err != nil || len(page.Chats) == 0 {
		t.Fatalf("no chats: %v", err)
	}
	return page.Chats[0].ID
}

func firstMessageID(t *testing.T, p *messaging.Provider, chatID, text string) string {
	t.Helper()
	page, err := p.Messages(context.Background(), chatID, messaging.MessagesQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range page.Messages {
		if m.Text != nil && *m.Text == text {
			return m.ID
		}
	}
	t.Fatalf("no message %q", text)
	return ""
}

func TestMessagingSendIsIdempotent(t *testing.T) {
	p, h := newProvider(t, messaging.GrantRead, messaging.GrantWrite)
	ctx := context.Background()
	h.Queue(bridgetest.TextMessage(bridgetest.DMKey(), "alice", "m1", "hi", time.UnixMilli(1_700_000_000_000)))
	chatID := firstChat(t, p)
	replyTo := firstMessageID(t, p, chatID, "hi")

	sent, err := p.Send(ctx, chatID, messaging.Send{Text: "hello back", ReplyTo: replyTo, ClientID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 1 || !sent.Messages[0].Outgoing || sent.Messages[0].ReplyTo == nil ||
		*sent.Messages[0].ReplyTo.MessageID != replyTo {
		t.Fatalf("unexpected send: %+v", sent)
	}
	again, err := p.Send(ctx, chatID, messaging.Send{Text: "hello back", ReplyTo: replyTo, ClientID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Messages[0].ID != sent.Messages[0].ID {
		t.Fatalf("retried send made a new message: %+v", again)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		m, err := p.Message(ctx, sent.Messages[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if *m.Status == messaging.StatusSent {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("message stayed %s", *m.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(h.Net.Sent()); n != 1 {
		t.Fatalf("network got %d messages", n)
	}

	files := []messaging.File{{Name: "a.txt", ContentType: "text/plain", Data: bytes.NewReader([]byte("x"))}}
	if _, err = p.Send(ctx, chatID, messaging.Send{Files: files}); errCode(err) != messaging.ErrNotSupported {
		t.Fatalf("expected files to be refused where the network takes none, got %v", err)
	}
	if err = p.React(ctx, replyTo, "👍"); errCode(err) != messaging.ErrNotSupported {
		t.Fatalf("expected reactions to be refused where the network takes none, got %v", err)
	}
	if err = p.Edit(ctx, replyTo, "not mine"); errCode(err) != messaging.ErrNotSupported {
		t.Fatalf("expected editing someone else's message to be refused, got %v", err)
	}
	if err = p.Edit(ctx, sent.Messages[0].ID, "hello again"); err != nil {
		t.Fatal(err)
	}
	if edits := h.Net.Edits(); len(edits) != 1 || edits[0].Content.Body != "hello again" {
		t.Fatalf("network got edits %+v", edits)
	}
	if err = p.MarkRead(ctx, chatID, replyTo); err != nil {
		t.Fatal(err)
	}
}

func TestMessagingEvents(t *testing.T) {
	p, h := newProvider(t, messaging.GrantRead)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := p.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if evt := <-stream; evt.Name != messaging.EventHello {
		t.Fatalf("expected hello first, got %+v", evt)
	}
	h.Queue(bridgetest.TextMessage(bridgetest.DMKey(), "alice", "m1", "hi", time.UnixMilli(1_700_000_000_000)))
	for evt := range stream {
		if evt.Name != messaging.EventChat {
			continue
		}
		var changed messaging.ChatChanged
		if err = json.Unmarshal(evt.Data, &changed); err != nil {
			t.Fatal(err)
		}
		if changed.ChatID != firstChat(t, p) {
			t.Fatalf("unexpected chat event: %s", evt.Data)
		}
		return
	}
	t.Fatal("stream ended without a chat event")
}
