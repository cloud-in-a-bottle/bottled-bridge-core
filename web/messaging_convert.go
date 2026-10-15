package web

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-in-a-bottle/messaging-service-spec/messaging"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// The service's IDs are the store's row IDs as decimal strings.
func svcID(id int64) string { return strconv.FormatInt(id, 10) }

func parseSvcID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, BadRequest("invalid id")
	}
	return id, nil
}

func svcPathID(r *http.Request) (int64, error) {
	return parseSvcID(r.PathValue("id"))
}

// Cursors are "<timestamp ms>.<row ID>" positions. Consumers treat them as opaque.
func positionCursor(sentAt, id int64) *string {
	c := fmt.Sprintf("%d.%d", sentAt, id)
	return &c
}

func parsePosition(raw string) (store.Position, error) {
	at, id, ok := strings.Cut(raw, ".")
	sentAt, err1 := strconv.ParseInt(at, 10, 64)
	rowID, err2 := strconv.ParseInt(id, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return store.Position{}, BadRequest("invalid cursor")
	}
	return store.Position{SentAt: sentAt, ID: rowID}, nil
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func optTime(ms int64) *time.Time {
	if ms == 0 {
		return nil
	}
	return new(msTime(ms))
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func avatarURL(sha string) *string {
	if sha == "" {
		return nil
	}
	return new("v1/avatars/" + sha)
}

func (s *Server) svcPerson(id int64, remoteID, name, avatarSHA string, isSelf bool) messaging.Person {
	p := messaging.Person{Name: optString(name), AvatarURL: avatarURL(avatarSHA), IsSelf: isSelf}
	if id != 0 {
		p.ID = svcID(id)
	}
	if store.IsNetworkPerson(remoteID) {
		p.Address = new(s.messaging.Network + ":" + remoteID)
		if s.messaging.Phone != nil {
			p.Phone = optString(s.messaging.Phone(remoteID))
		}
	}
	return p
}

func (s *Server) svcStorePerson(p store.Person) messaging.Person {
	return s.svcPerson(p.ID, p.RemoteID, p.Name, p.AvatarSHA256, p.IsSelf)
}

func (s *Server) svcChatFrom(c store.ConversationSummary) messaging.Chat {
	chat := messaging.Chat{
		ID:                 svcID(c.ID),
		Kind:               messaging.ChatGroup,
		Title:              c.Name,
		AvatarURL:          avatarURL(c.AvatarSHA256),
		LastMessageAt:      optTime(c.LastMessageAt),
		LastMessagePreview: optString(c.Preview),
		UnreadCount:        int(c.UnreadCount),
		MarkedUnread:       c.MarkedUnread,
		Pinned:             c.Pinned,
		Archived:           c.Archived,
		ReadOnly:           c.ReadOnly,
	}
	if c.Kind == string(store.KindDM) {
		chat.Kind = messaging.ChatDM
		if c.OtherPersonID != 0 {
			chat.OtherPerson = new(s.svcPerson(c.OtherPersonID, c.OtherRemoteID, c.OtherName, c.AvatarSHA256, false))
		}
	}
	switch {
	case c.MutedUntil < 0:
		chat.MutedUntil = new("forever")
	case c.MutedUntil > time.Now().UnixMilli():
		chat.MutedUntil = new(msTime(c.MutedUntil).Format(time.RFC3339Nano))
	}
	if c.DisappearMS > 0 {
		chat.DisappearingTimerS = new(int(c.DisappearMS / 1000))
	}
	return chat
}

func attachmentKind(a store.Attachment) messaging.AttachmentKind {
	switch {
	case a.IsSticker:
		return messaging.KindSticker
	case a.IsVoiceNote:
		return messaging.KindVoice
	case a.MsgType == "m.image":
		return messaging.KindImage
	case a.MsgType == "m.video":
		return messaging.KindVideo
	case a.MsgType == "m.audio":
		return messaging.KindAudio
	}
	return messaging.KindFile
}

func svcAttachment(a store.Attachment) messaging.Attachment {
	out := messaging.Attachment{
		ID:          svcID(a.ID),
		Kind:        attachmentKind(a),
		ContentType: optString(a.ContentType),
		Filename:    optString(a.Filename),
		Caption:     optString(a.Caption),
		Status:      messaging.AttachmentAvailable,
		URL:         "v1/attachments/" + svcID(a.ID),
	}
	if a.Size > 0 {
		out.Size = &a.Size
	}
	if a.Width > 0 && a.Height > 0 {
		out.Width, out.Height = new(int(a.Width)), new(int(a.Height))
	}
	if a.DurationMS > 0 {
		out.DurationMS = new(int(a.DurationMS))
	}
	switch a.Status {
	case "missing":
		out.Status = messaging.AttachmentMissing
	case "purged":
		out.Status = messaging.AttachmentExpired
	}
	return out
}

func (s *Server) svcMessageFrom(m store.Message) messaging.Message {
	out := messaging.Message{
		ID:          svcID(m.ID),
		ChatID:      svcID(m.ConversationID),
		Sender:      s.svcPerson(m.AuthorID, m.AuthorRemote, m.AuthorName, m.AuthorAvatar, m.Outgoing),
		Outgoing:    m.Outgoing,
		SentAt:      msTime(m.SentAt),
		Kind:        messaging.KindMessage,
		Attachments: []messaging.Attachment{},
		Reactions:   []messaging.Reaction{},
		EditedAt:    optTime(m.EditedAt),
		Unsent:      m.Deleted,
		ExpiresAt:   optTime(m.ExpiresAt),
	}
	if m.Kind == "update" {
		// As the UI shows them: notices are whole sentences, other updates follow the author's name.
		text := m.Body
		if m.MsgType != "notice" {
			name := m.AuthorName
			if m.Outgoing {
				name = "You"
			}
			text = strings.TrimSpace(name + " " + m.Body)
		}
		out.Kind = messaging.KindEvent
		out.Event = &messaging.Event{Type: cmp.Or(m.MsgType, "update"), Text: text}
		return out
	}
	if !m.Deleted {
		out.Text = optString(m.Body)
	}
	if m.Quote != nil {
		out.ReplyTo = &messaging.ReplyRef{
			MessageID: new(svcID(m.Quote.MessageID)), SenderName: optString(m.Quote.AuthorName),
			Text: optString(m.Quote.Text),
		}
	}
	for _, a := range m.Attachments {
		out.Attachments = append(out.Attachments, svcAttachment(a))
	}
	for _, r := range m.Reactions {
		out.Reactions = append(out.Reactions, messaging.Reaction{
			Emoji: r.Emoji, Sender: s.svcPerson(r.ReactorID, r.ReactorRemote, r.ReactorName, r.ReactorAvatar, r.Mine),
			Mine: r.Mine,
		})
	}
	if m.Status != "" {
		out.Status = new(messaging.Status(m.Status))
		out.Error = optString(m.Error)
	}
	return out
}

func (s *Server) svcMessagesFrom(msgs []store.Message) []messaging.Message {
	out := make([]messaging.Message, len(msgs))
	for i, m := range msgs {
		out[i] = s.svcMessageFrom(m)
	}
	return out
}

// svcCapabilities is what can be done in a conversation through the service: what the network allows there, plus
// what this app adds. ok is false for a chat with no network behind it, where nothing can be done.
func (s *Server) svcCapabilities(ctx context.Context, conversationID int64) (messaging.Capabilities, bool, error) {
	caps, ok, err := s.svc.Capabilities(ctx, conversationID)
	if err != nil || !ok {
		return messaging.Capabilities{}, false, err
	}
	return s.withAppCapabilities(caps), true, nil
}

func (s *Server) withAppCapabilities(caps messaging.Capabilities) messaging.Capabilities {
	caps.LoadOlder = s.branding.OnDemandHistory
	caps.DeliveryReceipts = s.messaging.DeliveryReceipts
	caps.StartChat = []string{"address"}
	if s.messaging.StartsChatsByPhone {
		caps.StartChat = append(caps.StartChat, "phone")
	}
	if caps.Attachments != nil && caps.Attachments.MaxSizeBytes > maxUploadBytes {
		caps.Attachments.MaxSizeBytes = maxUploadBytes
	}
	return caps
}
