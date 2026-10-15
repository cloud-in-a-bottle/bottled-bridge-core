package web

import (
	"cmp"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cloud-in-a-bottle/messaging-service-spec/messaging"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// svcLimit reads ?limit=, clamped to [1, most].
func svcLimit(r *http.Request, fallback, most int64) (int, error) {
	limit, err := QueryInt(r, "limit", fallback)
	if err != nil {
		return 0, err
	}
	return int(min(max(limit, 1), most)), nil
}

func svcState(a bridge.Account) messaging.AccountState {
	if !a.LoggedIn {
		return messaging.StateLoggedOut
	}
	switch a.State {
	case "CONNECTED", "BACKFILLING":
		return messaging.StateConnected
	case "STARTING", "RUNNING", "CONNECTING", "TRANSIENT_DISCONNECT":
		return messaging.StateConnecting
	case "LOGGED_OUT", "BAD_CREDENTIALS", "UNCONFIGURED":
		return messaging.StateLoggedOut
	}
	return messaging.StateError
}

func (s *Server) svcAccount(w http.ResponseWriter, r *http.Request) {
	account := s.svc.Account(r.Context())
	caps, err := s.svc.DefaultCapabilities(r.Context())
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	out := messaging.Account{
		Network:      s.messaging.Network,
		NetworkName:  s.branding.Network,
		State:        svcState(account),
		StateDetail:  optString(cmp.Or(account.StateMessage, account.StateError)),
		Syncing:      account.Syncing,
		Capabilities: s.withAppCapabilities(caps),
	}
	if self, err := s.store.Self(r.Context()); err == nil {
		out.Self = s.svcStorePerson(self)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.svcFail(w, r, err)
		return
	}
	WriteJSON(w, out)
}

func (s *Server) svcChats(w http.ResponseWriter, r *http.Request) {
	limit, err := svcLimit(r, 50, 500)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	archived := store.Archived(cmp.Or(r.URL.Query().Get("archived"), string(store.ArchivedExclude)))
	switch archived {
	case store.ArchivedExclude, store.ArchivedInclude, store.ArchivedOnly:
	default:
		s.svcFail(w, r, BadRequest("archived must be include, exclude or only"))
		return
	}
	var after *store.ChatPosition
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		pos, err := parsePosition(raw)
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		after = &store.ChatPosition{LastMessageAt: pos.SentAt, ID: pos.ID}
	}
	list, more, err := s.store.ConversationsPage(r.Context(), archived, after, limit)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	page := messaging.ChatPage{Chats: make([]messaging.Chat, len(list))}
	for i, c := range list {
		page.Chats[i] = s.svcChatFrom(c)
	}
	if more {
		last := list[len(list)-1]
		page.NextCursor = positionCursor(last.LastMessageAt, last.ID)
	}
	WriteJSON(w, page)
}

func (s *Server) svcChat(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	detail, err := s.store.ConversationDetail(r.Context(), id)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	out := messaging.ChatDetail{Chat: s.svcChatFrom(detail.Summary), Participants: []messaging.Participant{}}
	for _, m := range detail.Members {
		role := "member"
		if m.Membership == "invite" {
			role = "invited"
		}
		out.Participants = append(out.Participants, messaging.Participant{
			Person: s.svcPerson(m.PersonID, m.RemoteID, m.Name, m.AvatarSHA256, m.IsSelf), Role: role,
		})
	}
	caps, ok, err := s.svcCapabilities(r.Context(), id)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	if ok {
		out.Capabilities = &caps
	} else {
		// Imported history: nothing can be done in it.
		out.Capabilities = &messaging.Capabilities{}
	}
	WriteJSON(w, out)
}

func (s *Server) svcMessages(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	limit, err := svcLimit(r, 50, 500)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	if _, err = s.store.ConversationRoom(r.Context(), id); err != nil {
		s.svcFail(w, r, err)
		return
	}
	q := r.URL.Query()
	var page messaging.MessagePage
	var msgs []store.Message
	switch {
	case q.Get("after") != "":
		pos, err := parsePosition(q.Get("after"))
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		msgs, page.HasMoreAfter, err = s.store.After(r.Context(), id, pos, limit)
		page.HasMoreBefore = true
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
	case q.Get("around") != "":
		target, err := parseSvcID(q.Get("around"))
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		m, err := s.store.MessageByID(r.Context(), target)
		if err == nil && m.ConversationID != id {
			err = store.ErrNotFound
		}
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		// The message itself and what comes before it make up the first half of the page, the rest after it.
		pos := store.Position{SentAt: m.SentAt, ID: m.ID}
		var after []store.Message
		msgs, page.HasMoreBefore, err = s.store.Before(r.Context(), id,
			&store.Position{SentAt: pos.SentAt, ID: pos.ID + 1}, (limit+1)/2)
		if err == nil {
			after, page.HasMoreAfter, err = s.store.After(r.Context(), id, pos, max(limit-len(msgs), 1))
		}
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		msgs = append(msgs, after...)
	default:
		var before *store.Position
		if raw := q.Get("before"); raw != "" {
			pos, err := parsePosition(raw)
			if err != nil {
				s.svcFail(w, r, err)
				return
			}
			before = &pos
			page.HasMoreAfter = true
		}
		msgs, page.HasMoreBefore, err = s.store.Before(r.Context(), id, before, limit)
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
	}
	page.Messages = s.svcMessagesFrom(msgs)
	if len(msgs) > 0 {
		first, last := msgs[0], msgs[len(msgs)-1]
		page.BeforeCursor = positionCursor(first.SentAt, first.ID)
		page.AfterCursor = positionCursor(last.SentAt, last.ID)
	}
	WriteJSON(w, page)
}

func (s *Server) svcMessage(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	m, err := s.store.MessageByID(r.Context(), id)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	WriteJSON(w, s.svcMessageFrom(m))
}

func queryTime(r *http.Request, name string) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return 0, BadRequest("invalid " + name + ": want an ISO 8601 timestamp")
	}
	return t.UnixMilli(), nil
}

func (s *Server) svcRecent(w http.ResponseWriter, r *http.Request) {
	q := store.RecentQuery{}
	var err error
	if q.Limit, err = svcLimit(r, 50, 500); err != nil {
		s.svcFail(w, r, err)
		return
	}
	if q.Since, err = queryTime(r, "since"); err != nil {
		s.svcFail(w, r, err)
		return
	}
	if q.Until, err = queryTime(r, "until"); err != nil {
		s.svcFail(w, r, err)
		return
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		pos, err := parsePosition(raw)
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		q.Before = &pos
	}
	msgs, more, err := s.store.Recent(r.Context(), q)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	out := messaging.RecentMessages{Messages: s.svcMessagesFrom(msgs)}
	if more {
		last := msgs[len(msgs)-1]
		out.NextCursor = positionCursor(last.SentAt, last.ID)
	}
	WriteJSON(w, out)
}

func (s *Server) svcSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		s.svcFail(w, r, BadRequest("q is required"))
		return
	}
	limit, err := svcLimit(r, 50, 100)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	var chatID int64
	if raw := r.URL.Query().Get("chat_id"); raw != "" {
		if chatID, err = parseSvcID(raw); err != nil {
			s.svcFail(w, r, err)
			return
		}
	}
	hits, err := s.store.Search(r.Context(), query, chatID, limit)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.MessageID
	}
	msgs, err := s.store.MessagesByID(r.Context(), ids)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	byID := make(map[int64]store.Message, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}
	out := messaging.SearchResults{Hits: []messaging.SearchHit{}}
	for _, h := range hits {
		if m, ok := byID[h.MessageID]; ok {
			out.Hits = append(out.Hits, messaging.SearchHit{
				Message: s.svcMessageFrom(m), ChatTitle: h.ConversationName, Snippet: h.Snippet,
			})
		}
	}
	WriteJSON(w, out)
}

// svcPeople lists people this account knows whose names match, then (for a query, where the network can search)
// the network's matches that aren't among them.
func (s *Server) svcPeople(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	known, err := s.store.PeopleNamed(r.Context(), query, 50)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	out := messaging.People{People: []messaging.Person{}}
	seen := make(map[string]bool)
	for _, p := range known {
		out.People = append(out.People, s.svcStorePerson(p))
		seen[p.RemoteID] = true
	}
	if query != "" && s.svc.CanSearchPeople(r.Context()) {
		found, err := s.svc.SearchPeople(r.Context(), query)
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		for _, f := range found {
			if seen[f.RemoteID] {
				continue
			}
			seen[f.RemoteID] = true
			p, err := s.store.PersonByRemoteID(r.Context(), f.RemoteID)
			switch {
			case err == nil:
				out.People = append(out.People, s.svcStorePerson(p))
			case errors.Is(err, store.ErrNotFound):
				out.People = append(out.People, s.svcPerson(0, f.RemoteID, f.Name, "", false))
			default:
				s.svcFail(w, r, err)
				return
			}
		}
	}
	WriteJSON(w, out)
}
