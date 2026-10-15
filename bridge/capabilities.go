package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloud-in-a-bottle/messaging-service-spec/messaging"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// The room state the bridge keeps a chat's features in (event.RoomFeatures).
const roomFeaturesType = "com.beeper.room_features"

// Capabilities says what the network allows in a conversation, from the features the connector reported for it. ok is false
// if it reported none (eg imported history).
func (s *Service) Capabilities(ctx context.Context, conversationID int64) (caps messaging.Capabilities, ok bool, err error) {
	roomID, err := s.store.ConversationRoom(ctx, conversationID)
	if err != nil || strings.HasPrefix(roomID, "import:") {
		return caps, false, err
	}
	state, err := s.store.RoomState(ctx, roomID, roomFeaturesType)
	if err != nil {
		return caps, false, err
	}
	entry, found := state[""]
	if !found {
		return caps, false, nil
	}
	caps, err = s.capabilitiesFrom(entry.Content)
	return caps, err == nil, err
}

// DefaultCapabilities are those of the most recently active DM (else group): what a chat on this account can
// usually do. Empty before the connector has reported any.
func (s *Service) DefaultCapabilities(ctx context.Context) (messaging.Capabilities, error) {
	for _, kind := range []store.ConversationKind{store.KindDM, store.KindGroup} {
		content, err := s.store.LatestRoomState(ctx, kind, roomFeaturesType)
		if err != nil {
			return messaging.Capabilities{}, err
		} else if content != "" {
			return s.capabilitiesFrom(content)
		}
	}
	return messaging.Capabilities{}, nil
}

func (s *Service) capabilitiesFrom(content string) (messaging.Capabilities, error) {
	var f event.RoomFeatures
	if err := json.Unmarshal([]byte(content), &f); err != nil {
		return messaging.Capabilities{}, fmt.Errorf("room features: %w", err)
	}
	caps := messaging.Capabilities{
		MaxTextLength: f.MaxTextLength,
		Reply:         f.Reply.Partial(),
		ReadReceipts:  f.ReadReceipts,
		Typing:        f.TypingNotifications,
	}
	if f.Edit.Partial() {
		caps.Edit = &messaging.EditCapability{}
		if f.EditMaxAge != nil {
			caps.Edit.MaxAgeS = new(int(f.EditMaxAge.Seconds()))
		}
		if f.EditMaxCount > 0 {
			caps.Edit.MaxCount = new(f.EditMaxCount)
		}
	}
	if f.Delete.Partial() {
		caps.Unsend = &messaging.UnsendCapability{}
		if f.DeleteMaxAge != nil {
			caps.Unsend.MaxAgeS = new(int(f.DeleteMaxAge.Seconds()))
		}
	}
	if f.Reaction.Partial() {
		caps.Reactions = &messaging.ReactionCapability{MaxPerPerson: max(f.ReactionCount, 1), Emoji: f.AllowedReactions}
	}
	caps.Attachments = s.attachmentCapability(f.File)
	return caps, nil
}

// The kinds of file Send can send, by the msgtype it gives them (mediaMsgType).
var sendableKinds = []struct {
	msgType event.CapabilityMsgType
	kind    messaging.AttachmentKind
}{
	{event.MsgImage, messaging.KindImage},
	{event.MsgVideo, messaging.KindVideo},
	{event.MsgAudio, messaging.KindAudio},
	{event.MsgFile, messaging.KindFile},
}

func (s *Service) attachmentCapability(files event.FileFeatureMap) *messaging.AttachmentCapability {
	out := &messaging.AttachmentCapability{MaxPerMessage: 1}
	for _, k := range sendableKinds {
		ff := files[k.msgType]
		if ff == nil || !anySupported(ff.MimeTypes) {
			continue
		}
		out.Kinds = append(out.Kinds, k.kind)
		out.Caption = out.Caption || ff.Caption.Partial()
		out.MaxSizeBytes = max(out.MaxSizeBytes, ff.MaxSize)
	}
	if len(out.Kinds) == 0 {
		return nil
	}
	if out.MaxSizeBytes == 0 || out.MaxSizeBytes > s.opts.MaxFileSize {
		out.MaxSizeBytes = s.opts.MaxFileSize
	}
	return out
}

func anySupported(levels map[string]event.CapabilitySupportLevel) bool {
	for _, level := range levels {
		if level.Partial() {
			return true
		}
	}
	return false
}
