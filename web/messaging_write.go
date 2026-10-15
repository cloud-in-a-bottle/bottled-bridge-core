package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloud-in-a-bottle/messaging-service-spec/messaging"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// requireLogin fails writes while there's no account to send them from.
func (s *Server) requireLogin(ctx context.Context) error {
	if !s.svc.Account(ctx).LoggedIn {
		return bridge.ErrNotLoggedIn
	}
	return nil
}

// writableChat checks a conversation can be written to, and returns what can be done in it.
func (s *Server) writableChat(ctx context.Context, conversationID int64) (messaging.Capabilities, error) {
	if err := s.requireLogin(ctx); err != nil {
		return messaging.Capabilities{}, err
	}
	caps, ok, err := s.svcCapabilities(ctx, conversationID)
	if err != nil {
		return caps, err
	} else if !ok {
		roomID, err := s.store.ConversationRoom(ctx, conversationID)
		if err != nil {
			return caps, err
		} else if strings.HasPrefix(roomID, "import:") {
			return caps, store.ErrReadOnly
		}
		return caps, notSupported{"the network hasn't said what this chat allows yet"}
	}
	return caps, nil
}

// ownMessage loads one of the owner's messages for an edit or unsend, with what its chat allows.
func (s *Server) ownMessage(ctx context.Context, messageID int64) (store.Message, messaging.Capabilities, error) {
	m, err := s.store.MessageByID(ctx, messageID)
	if err != nil {
		return m, messaging.Capabilities{}, err
	}
	caps, err := s.writableChat(ctx, m.ConversationID)
	if err != nil {
		return m, caps, err
	}
	if !m.Outgoing || m.Kind != "message" {
		return m, caps, notSupported{"only your own messages can be changed"}
	}
	if m.Deleted {
		return m, caps, rejected{"the message was unsent"}
	}
	return m, caps, nil
}

func tooOld(sentAt int64, maxAgeS *int) bool {
	return maxAgeS != nil && time.Since(time.UnixMilli(sentAt)) > time.Duration(*maxAgeS)*time.Second
}

func (s *Server) svcStartChat(w http.ResponseWriter, r *http.Request) {
	var req messaging.StartChat
	if err := ReadJSON(r, &req); err != nil {
		s.svcFail(w, r, err)
		return
	}
	if err := s.requireLogin(r.Context()); err != nil {
		s.svcFail(w, r, err)
		return
	}
	var conversationID int64
	var err error
	switch {
	case req.Address != "":
		remoteID, ok := strings.CutPrefix(req.Address, s.messaging.Network+":")
		if !ok || !store.IsNetworkPerson(remoteID) {
			s.svcFail(w, r, BadRequest("not an address on "+s.messaging.Network))
			return
		}
		conversationID, err = s.svc.StartChatWithPerson(r.Context(), remoteID)
	case req.Phone != "":
		if !s.messaging.StartsChatsByPhone {
			s.svcFail(w, r, notSupported{"this network can't start chats by phone number"})
			return
		}
		conversationID, err = s.svc.StartChat(r.Context(), strings.TrimSpace(req.Phone))
	case req.PersonID != "":
		personID, err := parseSvcID(req.PersonID)
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		p, err := s.store.PersonByID(r.Context(), personID)
		if err != nil {
			s.svcFail(w, r, err)
			return
		} else if !store.IsNetworkPerson(p.RemoteID) {
			s.svcFail(w, r, notSupported{"not someone on " + s.messaging.Network})
			return
		}
		conversationID, err = s.svc.StartChatWithPerson(r.Context(), p.RemoteID)
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
	default:
		s.svcFail(w, r, BadRequest("give an address, phone or person_id"))
		return
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	summary, err := s.store.ConversationSummary(r.Context(), conversationID)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	WriteJSON(w, s.svcChatFrom(summary))
}

func (s *Server) svcSend(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err = r.ParseMultipartForm(32 << 20); err != nil {
		s.svcFail(w, r, BadRequest("invalid form: "+err.Error()))
		return
	}
	defer r.MultipartForm.RemoveAll()
	caps, err := s.writableChat(r.Context(), id)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	headers := r.MultipartForm.File["files[]"]
	if text == "" && len(headers) == 0 {
		s.svcFail(w, r, BadRequest("nothing to send"))
		return
	}
	if caps.MaxTextLength > 0 && utf8.RuneCountInString(text) > caps.MaxTextLength {
		s.svcFail(w, r, BadRequest("text is longer than this chat allows"))
		return
	}
	if len(headers) > 0 && caps.Attachments == nil {
		s.svcFail(w, r, notSupported{"this chat doesn't take files"})
		return
	}
	var replyTo int64
	if raw := r.FormValue("reply_to"); raw != "" {
		if !caps.Reply {
			s.svcFail(w, r, notSupported{"this chat doesn't take replies"})
			return
		}
		if replyTo, err = parseSvcID(raw); err != nil {
			s.svcFail(w, r, err)
			return
		}
		if target, err := s.store.MessageByID(r.Context(), replyTo); err != nil || target.ConversationID != id {
			s.svcFail(w, r, BadRequest("reply_to isn't a message in this chat"))
			return
		}
	}
	clientID := r.FormValue("client_id")

	// One send per client ID at a time, so a retry racing the original can't send twice.
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if clientID != "" {
		ids, found, err := s.store.SentFor(r.Context(), clientID)
		if err != nil {
			s.svcFail(w, r, err)
			return
		} else if found {
			s.writeSent(w, r, ids)
			return
		}
	}
	var files []bridge.OutgoingFile
	for _, header := range headers {
		f, err := s.storeUpload(header.Filename, header.Header.Get("Content-Type"), func() (io.ReadSeekCloser, error) {
			return header.Open()
		})
		if err != nil {
			s.svcFail(w, r, err)
			return
		}
		if f.Size > caps.Attachments.MaxSizeBytes {
			s.svcFail(w, r, BadRequest(f.Filename+" is larger than this chat allows"))
			return
		}
		if !slices.Contains(caps.Attachments.Kinds, outgoingKind(f.ContentType)) {
			s.svcFail(w, r, notSupported{"this chat doesn't take " + string(outgoingKind(f.ContentType)) + " files"})
			return
		}
		files = append(files, f)
	}
	ids, err := s.svc.SendAll(r.Context(), id, text, replyTo, files)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	if clientID != "" {
		if _, err = s.store.Tx(r.Context(), func(tx *store.Tx) error { return tx.RecordSent(clientID, ids) }); err != nil {
			s.svcFail(w, r, err)
			return
		}
	}
	s.writeSent(w, r, ids)
}

// outgoingKind is the kind of attachment bridge.Service.Send makes of a file.
func outgoingKind(contentType string) messaging.AttachmentKind {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return messaging.KindImage
	case strings.HasPrefix(contentType, "video/"):
		return messaging.KindVideo
	case strings.HasPrefix(contentType, "audio/"):
		return messaging.KindAudio
	}
	return messaging.KindFile
}

func (s *Server) writeSent(w http.ResponseWriter, r *http.Request, ids []int64) {
	msgs, err := s.store.MessagesByID(r.Context(), ids)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	WriteJSON(w, messaging.SentMessages{Messages: s.svcMessagesFrom(msgs)})
}

func (s *Server) svcEdit(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	var req struct {
		Text string `json:"text"`
	}
	if err == nil {
		err = ReadJSON(r, &req)
	}
	text := strings.TrimSpace(req.Text)
	if err == nil && text == "" {
		err = BadRequest("text is required")
	}
	var m store.Message
	var caps messaging.Capabilities
	if err == nil {
		m, caps, err = s.ownMessage(r.Context(), id)
	}
	switch {
	case err != nil:
	case caps.Edit == nil:
		err = notSupported{"this chat doesn't allow edits"}
	case tooOld(m.SentAt, caps.Edit.MaxAgeS):
		err = rejected{"too late to edit this message"}
	case caps.MaxTextLength > 0 && utf8.RuneCountInString(text) > caps.MaxTextLength:
		err = BadRequest("text is longer than this chat allows")
	default:
		err = s.svc.Edit(r.Context(), id, text)
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) svcUnsend(w http.ResponseWriter, r *http.Request) {
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
	if m.Status == "failed" {
		// Never reached the network: just discard it.
		if err = s.svc.Discard(r.Context(), id); err != nil {
			s.svcFail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m, caps, err := s.ownMessage(r.Context(), id)
	switch {
	case err != nil:
	case caps.Unsend == nil:
		err = notSupported{"this chat doesn't allow unsending"}
	case tooOld(m.SentAt, caps.Unsend.MaxAgeS):
		err = rejected{"too late to unsend this message"}
	default:
		err = s.svc.Unsend(r.Context(), id)
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) svcRetry(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	if err == nil {
		err = s.requireLogin(r.Context())
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	newID, err := s.svc.Retry(r.Context(), id)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	m, err := s.store.MessageByID(r.Context(), newID)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	WriteJSON(w, s.svcMessageFrom(m))
}

// reactable loads a message to react to, with what its chat allows for reactions.
func (s *Server) reactable(ctx context.Context, messageID int64) (*messaging.ReactionCapability, error) {
	m, err := s.store.MessageByID(ctx, messageID)
	if err != nil {
		return nil, err
	}
	caps, err := s.writableChat(ctx, m.ConversationID)
	if err != nil {
		return nil, err
	} else if caps.Reactions == nil {
		return nil, notSupported{"this chat doesn't take reactions"}
	} else if m.Kind != "message" || m.Deleted {
		return nil, notSupported{"this message can't be reacted to"}
	}
	return caps.Reactions, nil
}

func (s *Server) svcReact(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	var req struct {
		Emoji string `json:"emoji"`
	}
	if err == nil {
		err = ReadJSON(r, &req)
	}
	if err == nil && req.Emoji == "" {
		err = BadRequest("emoji is required")
	}
	var rc *messaging.ReactionCapability
	if err == nil {
		rc, err = s.reactable(r.Context(), id)
	}
	if err == nil && rc.Emoji != nil && !slices.Contains(rc.Emoji, req.Emoji) {
		err = BadRequest("this chat doesn't allow that reaction")
	}
	if err == nil {
		// Every network bridged so far allows one reaction per person, which React keeps to.
		err = s.svc.React(r.Context(), id, req.Emoji)
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) svcUnreact(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	if err == nil {
		_, err = s.reactable(r.Context(), id)
	}
	if err == nil {
		err = s.svc.Unreact(r.Context(), id, r.PathValue("emoji"))
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) svcRead(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	var req struct {
		UpTo string `json:"up_to"`
	}
	if r.ContentLength != 0 {
		if err = ReadJSON(r, &req); err != nil {
			s.svcFail(w, r, err)
			return
		}
	}
	var upTo int64
	if req.UpTo != "" {
		if upTo, err = parseSvcID(req.UpTo); err != nil {
			s.svcFail(w, r, err)
			return
		}
	}
	if err = s.requireLogin(r.Context()); err == nil {
		err = s.svc.MarkReadUpTo(r.Context(), id, upTo)
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) svcTyping(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	var req struct {
		Typing bool `json:"typing"`
	}
	if err == nil {
		err = ReadJSON(r, &req)
	}
	var caps messaging.Capabilities
	if err == nil {
		caps, err = s.writableChat(r.Context(), id)
	}
	if err == nil && !caps.Typing {
		err = notSupported{"this chat doesn't show typing"}
	}
	if err == nil {
		err = s.svc.SetTyping(r.Context(), id, req.Typing)
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) svcLoadOlder(w http.ResponseWriter, r *http.Request) {
	id, err := svcPathID(r)
	if err == nil && !s.branding.OnDemandHistory {
		err = notSupported{"this app doesn't fetch older history on demand"}
	}
	if err == nil {
		_, err = s.writableChat(r.Context(), id)
	}
	if err == nil {
		err = s.svc.LoadOlder(r.Context(), id)
		if errors.Is(err, bridge.ErrHistoryBusy) {
			// Already on its way: the chat event will come.
			err = nil
		}
	}
	if err != nil {
		s.svcFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
