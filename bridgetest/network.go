package bridgetest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

// FakeNetwork stands in for a network connector: tests feed remote events to the real bridgev2 portal logic, which
// talks to the real local connector and store. The owner is "me"; "alice" and "bob" are other people, and the chats
// are a DM with alice (DMKey) and a group with both (GroupKey).
type FakeNetwork struct {
	mu       sync.Mutex
	sent     []*bridgev2.MatrixMessage
	failSend bool
	// Forward history (what a chat brings when it first appears), and older history for backwards backfill, per
	// portal ID. Backwards pages are served newest first, one page per FetchMessages call.
	history map[networkid.PortalID][]*bridgev2.BackfillMessage
	older   map[networkid.PortalID][][]*bridgev2.BackfillMessage
}

func NewFakeNetwork() *FakeNetwork {
	return &FakeNetwork{
		history: map[networkid.PortalID][]*bridgev2.BackfillMessage{},
		older:   map[networkid.PortalID][][]*bridgev2.BackfillMessage{},
	}
}

// SetHistory sets the messages a chat brings when it first appears.
func (n *FakeNetwork) SetHistory(portal networkid.PortalID, msgs []*bridgev2.BackfillMessage) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.history[portal] = msgs
}

// SetOlderHistory sets pages of older messages for backwards backfill, the newest page first.
func (n *FakeNetwork) SetOlderHistory(portal networkid.PortalID, pages ...[]*bridgev2.BackfillMessage) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.older[portal] = pages
}

// SetFailSend makes sending fail (or work again).
func (n *FakeNetwork) SetFailSend(fail bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failSend = fail
}

// Sent is what's been handed to the network to send.
func (n *FakeNetwork) Sent() []*bridgev2.MatrixMessage {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]*bridgev2.MatrixMessage(nil), n.sent...)
}

func (n *FakeNetwork) Init(*bridgev2.Bridge)            {}
func (n *FakeNetwork) Start(context.Context) error      { return nil }
func (n *FakeNetwork) GetBridgeInfoVersion() (int, int) { return 1, 1 }
func (n *FakeNetwork) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{DisplayName: "Fake", NetworkID: "fake", BeeperBridgeType: "fake"}
}
func (n *FakeNetwork) GetDBMetaTypes() database.MetaTypes { return database.MetaTypes{} }
func (n *FakeNetwork) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}
func (n *FakeNetwork) GetConfig() (string, any, configupgrade.Upgrader) { return "", nil, nil }
func (n *FakeNetwork) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{ID: "qr", Name: "QR"}}
}

// CreateLogin starts a QR login whose wait only ends when it's cancelled, like a QR code nobody scans.
func (n *FakeNetwork) CreateLogin(_ context.Context, _ *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != "qr" {
		return nil, fmt.Errorf("no such flow %q", flowID)
	}
	return &fakeQRLogin{cancelled: make(chan struct{})}, nil
}

type fakeQRLogin struct {
	cancelOnce sync.Once
	cancelled  chan struct{}
}

func (l *fakeQRLogin) Start(context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type: bridgev2.LoginStepTypeDisplayAndWait, StepID: "qr", Instructions: "Scan the code",
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{Type: bridgev2.LoginDisplayTypeQR, Data: "fake-qr"},
	}, nil
}

func (l *fakeQRLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	select {
	case <-l.cancelled:
		return nil, fmt.Errorf("login cancelled")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *fakeQRLogin) Cancel() {
	l.cancelOnce.Do(func() { close(l.cancelled) })
}
func (n *FakeNetwork) LoadUserLogin(_ context.Context, login *bridgev2.UserLogin) error {
	login.Client = &fakeClient{n: n}
	return nil
}

type fakeClient struct {
	n *FakeNetwork
}

var (
	_ bridgev2.BackfillingNetworkAPI       = (*fakeClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI = (*fakeClient)(nil)
)

func (c *fakeClient) Connect(context.Context)      {}
func (c *fakeClient) Disconnect()                  {}
func (c *fakeClient) IsLoggedIn() bool             { return true }
func (c *fakeClient) LogoutRemote(context.Context) {}
func (c *fakeClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return userID == "me"
}

func members(ids ...networkid.UserID) *bridgev2.ChatMemberList {
	list := &bridgev2.ChatMemberList{IsFull: true, MemberMap: bridgev2.ChatMemberMap{}}
	for _, userID := range ids {
		list.MemberMap.Set(bridgev2.ChatMember{EventSender: bridgev2.EventSender{Sender: userID, IsFromMe: userID == "me"}})
	}
	return list
}

func (c *fakeClient) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	if portal.ID == GroupKey().ID {
		name := "Weekend plans"
		return &bridgev2.ChatInfo{
			Name: &name, Type: new(database.RoomTypeDefault), Members: members("me", "alice", "bob"), CanBackfill: true,
		}, nil
	}
	info := &bridgev2.ChatInfo{Type: new(database.RoomTypeDM), Members: members("me", "alice"), CanBackfill: true}
	info.Members.OtherUserID = "alice"
	return info, nil
}

func (c *fakeClient) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	names := map[networkid.UserID]string{"me": "Me Myself", "alice": "Alice", "bob": "Bob"}
	name := names[ghost.ID]
	return &bridgev2.UserInfo{Name: &name}, nil
}

func (c *fakeClient) GetCapabilities(context.Context, *bridgev2.Portal) *event.RoomFeatures {
	return &event.RoomFeatures{Reply: event.CapLevelFullySupported, Edit: event.CapLevelFullySupported, Delete: event.CapLevelFullySupported}
}

func (c *fakeClient) HandleMatrixMessage(_ context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	if c.n.failSend {
		return nil, fmt.Errorf("the network said no")
	}
	c.n.sent = append(c.n.sent, msg)
	return &bridgev2.MatrixMessageResponse{DB: &database.Message{
		ID: networkid.MessageID(fmt.Sprintf("sent-%d", len(c.n.sent))), SenderID: "me", Timestamp: time.Now(),
	}}, nil
}

func (c *fakeClient) HandleMatrixMessageRemove(context.Context, *bridgev2.MatrixMessageRemove) error {
	return nil
}

func (c *fakeClient) FetchMessages(_ context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	if params.Forward {
		return &bridgev2.FetchMessagesResponse{Messages: c.n.history[params.Portal.ID], Forward: true}, nil
	}
	pages := c.n.older[params.Portal.ID]
	if len(pages) == 0 {
		return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
	}
	c.n.older[params.Portal.ID] = pages[1:]
	return &bridgev2.FetchMessagesResponse{Messages: pages[0], HasMore: len(pages) > 1}, nil
}
