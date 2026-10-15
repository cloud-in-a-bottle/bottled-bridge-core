// Package bridge runs a bridgev2 network connector inside a bridge whose "Matrix" side is the local message store,
// and turns what the web UI asks for into events for the bridge.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// Options configure the bridge for one network.
type Options struct {
	// The network connector, configured but not yet initialized.
	Connector bridgev2.NetworkConnector
	// Marks the bridge database as this app's, so it isn't mistaken for another program's. Keep it stable: it's
	// checked against existing databases.
	DBOwner string
	// The largest attachment the bridge will download.
	MaxFileSize int64
	// Adjusts DefaultConfig, eg to turn on the backfill queue for a network where it costs no network traffic.
	Configure func(*bridgeconfig.BridgeConfig)
	// Searches the network for people, if the connector's own search (bridgev2.UserSearchingNetworkAPI) isn't what
	// the app wants. Nil uses the connector's.
	SearchPeople func(ctx context.Context, login *bridgev2.UserLogin, query string) ([]FoundPerson, error)
	// Drops and re-establishes the connection. Nil disconnects and connects the login's client.
	Reconnect func(login *bridgev2.UserLogin)
}

type Service struct {
	br     *bridgev2.Bridge
	opts   Options
	mx     *localmx.Connector
	store  *store.Store
	files  *files.Store
	broker *events.Broker
	log    zerolog.Logger

	loginMu sync.Mutex
	login   *pendingLogin

	historyMu sync.Mutex
	// Conversations whose older history is being fetched.
	historyBusy map[int64]bool
}

// DefaultConfig is conservative about traffic: it keeps what the bridge asks of the network to about what a person
// using a normal client causes.
func DefaultConfig() *bridgeconfig.BridgeConfig {
	return &bridgeconfig.BridgeConfig{
		// Nothing typed in the UI should be taken as a bridge command.
		CommandPrefix: "\x00bridge-command",
		Permissions: bridgeconfig.PermissionConfig{
			localmx.ServerName: &bridgeconfig.PermissionLevelAdmin,
		},
		// Handle each remote event fully (including the store write) before acknowledging it.
		PortalEventBuffer: 0,
		// Kept to about what opening a chat loads (one page), so a first sync of many chats isn't a burst of history
		// requests. Catch-up after downtime only asks for what was actually missed.
		Backfill: bridgeconfig.BackfillConfig{
			Enabled:            true,
			MaxInitialMessages: 20,
			MaxCatchupMessages: 200,
			Threads:            bridgeconfig.BackfillThreadsConfig{MaxInitialMessages: 0},
			// Off: working backwards through every chat's whole history is traffic no normal client makes, and a
			// likelier way to draw the network's attention to the account. The bridge still records a backfill task
			// per chat, so turning this on later picks up every chat, and LoadOlder uses it one page at a time.
			Queue: bridgeconfig.BackfillQueueConfig{
				Enabled:    false,
				BatchSize:  100,
				BatchDelay: 20,
				MaxBatches: -1,
			},
		},
		// Logging out keeps the chats, so logging in again (eg after the network ends the session) carries on with
		// them.
		CleanupOnLogout: bridgeconfig.CleanupOnLogouts{Enabled: false},
		// A message sent during a brief disconnection waits for the reconnect rather than failing straight away.
		SendReconnectWait: 30 * time.Second,
	}
}

func New(opts Options, bridgeDBPath string, st *store.Store, fs *files.Store, broker *events.Broker, log zerolog.Logger) (*Service, error) {
	if opts.Connector == nil || opts.DBOwner == "" || opts.MaxFileSize <= 0 {
		return nil, errors.New("bridge options need a connector, a database owner and a maximum file size")
	}
	db, err := dbutil.NewFromConfig(opts.DBOwner, dbutil.Config{
		PoolConfig: dbutil.PoolConfig{
			Type:         "sqlite3-fk-wal",
			URI:          fmt.Sprintf("file:%s?_txlock=immediate", bridgeDBPath),
			MaxOpenConns: 5,
			MaxIdleConns: 1,
		},
	}, dbutil.ZeroLogger(log.With().Str("db_section", "bridge").Logger()))
	if err != nil {
		return nil, fmt.Errorf("failed to open bridge database: %w", err)
	}
	cfg := DefaultConfig()
	if opts.Configure != nil {
		opts.Configure(cfg)
	}
	mx := localmx.New(st, fs, broker, log.With().Str("component", "localmx").Logger())
	br := bridgev2.NewBridge("", db, log, cfg, mx, opts.Connector, commands.NewProcessor)
	if sizer, ok := opts.Connector.(bridgev2.MaxFileSizeingNetwork); ok {
		sizer.SetMaxFileSize(opts.MaxFileSize)
	}
	return &Service{
		br: br, opts: opts, mx: mx, store: st, files: fs, broker: broker, log: log, historyBusy: make(map[int64]bool),
	}, nil
}

func (s *Service) Start(ctx context.Context) error {
	if err := s.br.Start(ctx); err != nil {
		return err
	}
	if login := s.CurrentLogin(ctx); login != nil {
		return s.recordSelf(ctx, login)
	}
	return nil
}

func (s *Service) Stop() {
	s.br.Stop()
}

// Bridge is the underlying bridge, for app code that needs the connector's own API.
func (s *Service) Bridge() *bridgev2.Bridge {
	return s.br
}

func (s *Service) user(ctx context.Context) (*bridgev2.User, error) {
	user, err := s.br.GetUserByMXID(ctx, localmx.OwnerMXID)
	if err != nil {
		return nil, err
	} else if user == nil {
		return nil, fmt.Errorf("bridge refused the owner user")
	}
	return user, nil
}

// CurrentLogin is the account on the network, or nil before one is logged in. There is only ever one.
func (s *Service) CurrentLogin(ctx context.Context) *bridgev2.UserLogin {
	user, err := s.user(ctx)
	if err != nil {
		s.log.Err(err).Msg("Failed to get owner user")
		return nil
	}
	return user.GetDefaultLogin()
}

// AddLogin records an account that logged in without going through StartLogin: the tests' fake network.
func (s *Service) AddLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	return s.recordSelf(ctx, login)
}

func (s *Service) recordSelf(ctx context.Context, login *bridgev2.UserLogin) error {
	_, err := s.store.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.SetSelf(string(login.ID)); err != nil {
			return err
		}
		if login.RemoteName != "" {
			return tx.SetPersonName(string(login.ID), login.RemoteName)
		}
		return nil
	})
	return err
}

// Account is the logged-in account and its connection state.
type Account struct {
	LoggedIn bool   `json:"logged_in"`
	RemoteID string `json:"remote_id,omitempty"`
	Name     string `json:"name,omitempty"`
	// From the bridge: CONNECTED, CONNECTING, TRANSIENT_DISCONNECT, BAD_CREDENTIALS, UNKNOWN_ERROR, LOGGED_OUT, ...
	State        string `json:"state"`
	StateError   string `json:"state_error,omitempty"`
	StateMessage string `json:"state_message,omitempty"`
}

func (s *Service) Account(ctx context.Context) Account {
	login := s.CurrentLogin(ctx)
	if login == nil {
		return Account{State: "LOGGED_OUT"}
	}
	st := s.mx.BridgeStatus()
	return Account{
		LoggedIn:     true,
		RemoteID:     string(login.ID),
		Name:         login.RemoteName,
		State:        string(st.StateEvent),
		StateError:   string(st.Error),
		StateMessage: st.Message,
	}
}

func (s *Service) Logout(ctx context.Context) error {
	login := s.CurrentLogin(ctx)
	if login == nil {
		return nil
	}
	login.Logout(ctx)
	return nil
}

// Reconnect drops and re-establishes the connection to the network, eg after an unknown error.
func (s *Service) Reconnect(ctx context.Context) error {
	login := s.CurrentLogin(ctx)
	if login == nil {
		return ErrNotLoggedIn
	}
	if s.opts.Reconnect != nil {
		s.opts.Reconnect(login)
		return nil
	}
	login.Client.Disconnect()
	login.Client.Connect(login.Log.WithContext(s.br.BackgroundCtx))
	return nil
}
