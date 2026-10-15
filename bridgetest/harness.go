// Package bridgetest runs the real bridge, local connector and store against a fake network, for the core's tests
// and for apps' (eg to seed a development database).
package bridgetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

type Harness struct {
	T      *testing.T
	Ctx    context.Context
	Net    *FakeNetwork
	Bridge *bridgev2.Bridge
	Store  *store.Store
	Files  *files.Store
	Login  *bridgev2.UserLogin
	Broker *events.Broker
	Svc    *bridge.Service
}

func New(t *testing.T) *Harness {
	return NewIn(t, t.TempDir(), Options{})
}

type Options struct {
	// The bridge database's owner, as in bridge.Options; it must match the app's for the app to open the database.
	// Defaults to "bridgetest".
	DBOwner string
	// Adjusts the bridge config, as bridge.Options.Configure does.
	Configure func(*bridgeconfig.BridgeConfig)
}

// NewIn lays out dir like an app's data: sqlite/{main,bridge}.db and data/attachments.
func NewIn(t *testing.T, dir string, hopts Options) *Harness {
	log := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.InfoLevel)
	if err := os.MkdirAll(filepath.Join(dir, "sqlite"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "sqlite", "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fs, err := files.New(filepath.Join(dir, "data", "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	broker := events.NewBroker()
	net := NewFakeNetwork()
	if hopts.DBOwner == "" {
		hopts.DBOwner = "bridgetest"
	}
	opts := bridge.Options{Connector: net, DBOwner: hopts.DBOwner, MaxFileSize: 1 << 20, Configure: hopts.Configure}
	svc, err := bridge.New(opts, filepath.Join(dir, "sqlite", "bridge.db"), st, fs, broker, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx := log.WithContext(context.Background())
	if err = svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)
	br := svc.Bridge()
	user, err := br.GetUserByMXID(ctx, localmx.OwnerMXID)
	if err != nil {
		t.Fatal(err)
	}
	login := user.GetDefaultLogin()
	if login == nil {
		if login, err = user.NewLogin(ctx, &database.UserLogin{ID: "me", RemoteName: "Me Myself"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = svc.AddLogin(ctx, login); err != nil {
		t.Fatal(err)
	}
	return &Harness{T: t, Ctx: ctx, Net: net, Bridge: br, Store: st, Files: fs, Login: login, Broker: broker, Svc: svc}
}

func DMKey() networkid.PortalKey    { return networkid.PortalKey{ID: "alice", Receiver: "me"} }
func GroupKey() networkid.PortalKey { return networkid.PortalKey{ID: "group", Receiver: "me"} }

// Sender is the EventSender for one of the fake network's users.
func Sender(userID networkid.UserID) bridgev2.EventSender {
	return bridgev2.EventSender{Sender: userID, IsFromMe: userID == "me"}
}

func TextMessage(key networkid.PortalKey, sender networkid.UserID, msgID networkid.MessageID, body string, ts time.Time) *simplevent.Message[string] {
	return &simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    key,
			Sender:       Sender(sender),
			CreatePortal: true,
			Timestamp:    ts,
		},
		ID:   msgID,
		Data: body,
		ConvertMessageFunc: func(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, data string) (*bridgev2.ConvertedMessage, error) {
			return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
				Type: event.EventMessage, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: data},
			}}}, nil
		},
	}
}

// BackfillText is a text message as a backfill batch carries it.
func BackfillText(sender networkid.UserID, msgID networkid.MessageID, body string, ts time.Time) *bridgev2.BackfillMessage {
	return &bridgev2.BackfillMessage{
		ConvertedMessage: &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
			Type: event.EventMessage, Content: &event.MessageEventContent{MsgType: event.MsgText, Body: body},
		}}},
		Sender: Sender(sender), ID: msgID, Timestamp: ts,
	}
}

// ImageMessage converts to a photo with a caption, uploaded through the intent like a real connector does.
func ImageMessage(sender networkid.UserID, msgID networkid.MessageID, png []byte, caption string, ts time.Time) *simplevent.Message[string] {
	msg := TextMessage(DMKey(), sender, msgID, "", ts)
	msg.ConvertMessageFunc = func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, _ string) (*bridgev2.ConvertedMessage, error) {
		uri, _, err := intent.UploadMedia(ctx, portal.MXID, png, "photo.png", "image/png")
		if err != nil {
			return nil, err
		}
		body := "photo.png"
		if caption != "" {
			body = caption
		}
		return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgImage, Body: body, FileName: "photo.png", URL: uri,
				Info: &event.FileInfo{MimeType: "image/png", Size: len(png), Width: 180, Height: 180},
			},
		}}}, nil
	}
	return msg
}

// Queue hands a remote event to the bridge and waits for it to be handled.
func (h *Harness) Queue(evt bridgev2.RemoteEvent) {
	h.T.Helper()
	res := h.Login.QueueRemoteEvent(evt)
	if !res.Success {
		h.T.Fatalf("remote event not handled: %+v", res)
	}
}

func (h *Harness) ConversationID(key networkid.PortalKey) int64 {
	h.T.Helper()
	id, err := h.Store.ConversationByRoom(h.Ctx, string(localmx.RoomIDFor(key)))
	if err != nil {
		h.T.Fatalf("conversation for %v: %v", key, err)
	}
	return id
}

func (h *Harness) Messages(key networkid.PortalKey) []store.Message {
	h.T.Helper()
	page, err := h.Store.MessagesPage(h.Ctx, h.ConversationID(key), 0, 100)
	if err != nil {
		h.T.Fatal(err)
	}
	return page.Messages
}

// WaitFor polls the newest message in a conversation until check accepts it.
func (h *Harness) WaitFor(key networkid.PortalKey, check func(m store.Message) bool) store.Message {
	h.T.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs := h.Messages(key)
		if len(msgs) > 0 && check(msgs[len(msgs)-1]) {
			return msgs[len(msgs)-1]
		}
		if time.Now().After(deadline) {
			h.T.Fatalf("timed out; messages: %+v", msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
