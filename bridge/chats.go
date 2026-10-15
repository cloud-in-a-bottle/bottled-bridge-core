package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
)

var (
	ErrNotLoggedIn = errors.New("not logged in")
	ErrNoSearch    = errors.New("this network can't search for people")
)

// StartChat opens (creating if needed) the one-to-one chat with a user, given their remote ID or anything else the
// connector resolves (eg a phone number), and returns its conversation ID.
func (s *Service) StartChat(ctx context.Context, identifier string) (int64, error) {
	login := s.CurrentLogin(ctx)
	if login == nil {
		return 0, ErrNotLoggedIn
	}
	api, ok := login.Client.(bridgev2.IdentifierResolvingNetworkAPI)
	if !ok {
		return 0, errors.New("the connector can't start chats")
	}
	resp, err := api.ResolveIdentifier(ctx, identifier, true)
	if err != nil {
		return 0, err
	} else if resp == nil || resp.Chat == nil {
		return 0, fmt.Errorf("couldn't find a chat with %s", identifier)
	}
	portal := resp.Chat.Portal
	if portal == nil {
		if portal, err = s.br.GetPortalByKey(ctx, resp.Chat.PortalKey); err != nil {
			return 0, err
		}
	}
	if err = portal.CreateMatrixRoom(ctx, login, resp.Chat.PortalInfo); err != nil {
		return 0, err
	}
	return s.store.ConversationByRoom(ctx, string(portal.MXID))
}

// FoundPerson is a user found by searching the network.
type FoundPerson struct {
	RemoteID string `json:"remote_id"`
	Name     string `json:"name"`
	// You already have a one-to-one chat with them on the network.
	HasChat bool `json:"has_chat"`
	// The network counts them as connected to you (a contact, a friend), not just anyone on it.
	IsContact bool `json:"is_contact"`
	// What the network shows under the name, eg "Friends on Facebook" or a phone number.
	Context string `json:"context,omitempty"`
}

// CanSearchPeople reports whether SearchPeople can work for this network.
func (s *Service) CanSearchPeople(ctx context.Context) bool {
	if s.opts.SearchPeople != nil {
		return true
	}
	login := s.CurrentLogin(ctx)
	if login == nil {
		return false
	}
	_, ok := login.Client.(bridgev2.UserSearchingNetworkAPI)
	return ok
}

// SearchPeople searches the network for people to message.
func (s *Service) SearchPeople(ctx context.Context, query string) ([]FoundPerson, error) {
	login := s.CurrentLogin(ctx)
	if login == nil {
		return nil, ErrNotLoggedIn
	}
	if s.opts.SearchPeople != nil {
		return s.opts.SearchPeople(ctx, login, query)
	}
	api, ok := login.Client.(bridgev2.UserSearchingNetworkAPI)
	if !ok {
		return nil, ErrNoSearch
	}
	results, err := api.SearchUsers(ctx, query)
	if err != nil {
		return nil, err
	}
	found := []FoundPerson{}
	for _, r := range results {
		if r == nil || r.UserID == "" {
			continue
		}
		p := FoundPerson{RemoteID: string(r.UserID), HasChat: r.Chat != nil && r.Chat.Portal != nil && r.Chat.Portal.MXID != ""}
		if r.UserInfo != nil && r.UserInfo.Name != nil {
			p.Name = *r.UserInfo.Name
		} else if r.Ghost != nil {
			p.Name = r.Ghost.Name
		}
		if p.Name == "" {
			p.Name = p.RemoteID
		}
		if r.UserInfo != nil && len(r.UserInfo.Identifiers) > 0 {
			// URIs like "tel:+15551234567".
			_, p.Context, _ = strings.Cut(r.UserInfo.Identifiers[0], ":")
		}
		found = append(found, p)
	}
	return found, nil
}
