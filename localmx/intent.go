package localmx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// intent acts as one Matrix user: the bridge bot, the owner (as their double puppet), or a ghost.
type intent struct {
	c     *Connector
	mxid  id.UserID
	ghost networkid.UserID
}

var _ bridgev2.MatrixAPI = (*intent)(nil)

func (c *Connector) isSelf(userID networkid.UserID) bool {
	return c.br.GetCachedUserLoginByID(networkid.UserLoginID(userID)) != nil
}

func (in *intent) GetMXID() id.UserID {
	return in.mxid
}

func (in *intent) IsDoublePuppet() bool {
	return in.mxid == OwnerMXID
}

func (in *intent) SendMessage(ctx context.Context, roomID id.RoomID, eventType event.Type, content *event.Content, extra *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	eventID, err := in.c.sendTimelineEvent(ctx, in.mxid, roomID, eventType, content, extra)
	if err != nil {
		return nil, err
	}
	return &mautrix.RespSendEvent{EventID: eventID}, nil
}

func (in *intent) SendState(ctx context.Context, roomID id.RoomID, eventType event.Type, stateKey string, content *event.Content, ts time.Time) (*mautrix.RespSendEvent, error) {
	eventID, err := in.c.sendState(ctx, in.mxid, roomID, eventType, stateKey, content, ts)
	if err != nil {
		return nil, err
	}
	return &mautrix.RespSendEvent{EventID: eventID}, nil
}

// MarkRead is the owner reading on another device, or someone else's read receipt.
func (in *intent) MarkRead(ctx context.Context, roomID id.RoomID, eventID id.EventID, ts time.Time) error {
	return in.c.write(ctx, func(tx *store.Tx) error {
		ref, err := tx.PartByEvent(string(eventID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if in.mxid == OwnerMXID {
			return tx.SetLastRead(ref.ConversationID, ref.SentAt)
		}
		personID, err := in.c.personFor(ctx, tx, in.mxid)
		if err != nil {
			return err
		}
		at := timestampMS(ts)
		if at == 0 {
			at = nowMS()
		}
		return tx.SetReadReceipt(ref.ConversationID, personID, ref.SentAt, at)
	})
}

func (in *intent) MarkUnread(ctx context.Context, roomID id.RoomID, unread bool) error {
	return in.c.updateConversation(ctx, roomID, func(tx *store.Tx, conversationID int64) error {
		return tx.SetConversationMarkedUnread(conversationID, unread)
	})
}

func (in *intent) MarkTyping(ctx context.Context, roomID id.RoomID, typingType bridgev2.TypingType, timeout time.Duration) error {
	if in.mxid == OwnerMXID || in.mxid == BotMXID {
		return nil
	}
	var conversationID, personID int64
	_, err := in.c.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		if conversationID, _, err = tx.ConversationByRoom(string(roomID)); err != nil {
			return err
		}
		personID, err = in.c.personFor(ctx, tx, in.mxid)
		return err
	})
	if err != nil {
		return err
	}
	in.c.broker.Publish("typing", events.Typing{ConversationID: conversationID, PersonID: personID, Started: timeout > 0})
	return nil
}

func (in *intent) DownloadMedia(ctx context.Context, uri id.ContentURIString, file *event.EncryptedFileInfo) ([]byte, error) {
	sha := SHAFromContentURI(uri)
	if sha == "" {
		return nil, fmt.Errorf("unknown media %s", uri)
	}
	return in.c.files.Read(sha)
}

func (in *intent) DownloadMediaToFile(ctx context.Context, uri id.ContentURIString, file *event.EncryptedFileInfo, writable bool, callback func(*os.File) error) error {
	sha := SHAFromContentURI(uri)
	if sha == "" {
		return fmt.Errorf("unknown media %s", uri)
	}
	src, err := os.Open(in.c.files.Path(sha))
	if err != nil {
		return err
	}
	defer src.Close()
	if !writable {
		return callback(src)
	}
	tmp, err := os.CreateTemp("", "media-")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if _, err = io.Copy(tmp, src); err != nil {
		return err
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return callback(tmp)
}

func (in *intent) UploadMedia(ctx context.Context, roomID id.RoomID, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	sha, err := in.c.files.Put(data)
	if err != nil {
		return "", nil, err
	}
	return ContentURI(sha), nil, nil
}

func (in *intent) UploadMediaStream(ctx context.Context, roomID id.RoomID, size int64, requireFile bool, cb bridgev2.FileStreamCallback) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	tmp, err := os.CreateTemp("", "upload-")
	if err != nil {
		return "", nil, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	res, err := cb(tmp)
	if err != nil {
		return "", nil, bridgev2.CallbackError{Type: "write", Wrapped: err}
	}
	path := tmp.Name()
	if res.ReplacementFile != "" {
		path = res.ReplacementFile
		defer os.Remove(res.ReplacementFile)
	}
	src, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer src.Close()
	sha, err := in.c.files.PutStream(func(w io.Writer) error {
		_, err := io.Copy(w, src)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	return ContentURI(sha), nil, nil
}

func (in *intent) SetDisplayName(ctx context.Context, name string) error {
	if in.ghost == "" {
		return nil
	}
	return in.c.write(ctx, func(tx *store.Tx) error {
		return tx.SetPersonName(string(in.ghost), name)
	})
}

func (in *intent) SetAvatarURL(ctx context.Context, avatarURL id.ContentURIString) error {
	if in.ghost == "" {
		return nil
	}
	return in.c.write(ctx, func(tx *store.Tx) error {
		return tx.SetPersonAvatar(string(in.ghost), SHAFromContentURI(avatarURL))
	})
}

func (in *intent) SetExtraProfileMeta(ctx context.Context, data any) error {
	return nil
}

func (in *intent) SetProfile(ctx context.Context, data any) error {
	return nil
}

func (in *intent) CreateRoom(ctx context.Context, req *mautrix.ReqCreateRoom) (id.RoomID, error) {
	return in.c.createRoom(ctx, req)
}

// DeleteRoom hides the conversation but keeps its history: the bridge also deletes rooms it replaces, eg when a
// chat moves to end-to-end encryption.
func (in *intent) DeleteRoom(ctx context.Context, roomID id.RoomID, puppetsOnly bool) error {
	if puppetsOnly {
		return nil
	}
	return in.c.write(ctx, func(tx *store.Tx) error {
		return tx.MarkConversationDeleted(string(roomID))
	})
}

func (in *intent) EnsureJoined(ctx context.Context, roomID id.RoomID, params ...bridgev2.EnsureJoinedParams) error {
	if in.mxid == BotMXID {
		return nil
	}
	members, err := in.c.GetMembers(ctx, roomID)
	if err != nil {
		return err
	}
	if current, ok := members[in.mxid]; ok && current.Membership == event.MembershipJoin {
		return nil
	}
	_, err = in.SendState(ctx, roomID, event.StateMember, string(in.mxid), &event.Content{
		Parsed: &event.MemberEventContent{Membership: event.MembershipJoin},
	}, time.Time{})
	return err
}

func (in *intent) EnsureInvited(ctx context.Context, roomID id.RoomID, userID id.UserID) error {
	members, err := in.c.GetMembers(ctx, roomID)
	if err != nil {
		return err
	}
	if current, ok := members[userID]; ok && (current.Membership == event.MembershipJoin || current.Membership == event.MembershipInvite) {
		return nil
	}
	_, err = in.SendState(ctx, roomID, event.StateMember, string(userID), &event.Content{
		Parsed: &event.MemberEventContent{Membership: event.MembershipInvite},
	}, time.Time{})
	return err
}

func (in *intent) TagRoom(ctx context.Context, roomID id.RoomID, tag event.RoomTag, isTagged bool) error {
	return in.c.updateConversation(ctx, roomID, func(tx *store.Tx, conversationID int64) error {
		switch tag {
		case event.RoomTagFavourite:
			return tx.SetConversationPinned(conversationID, isTagged)
		case event.RoomTagLowPriority:
			return tx.SetConversationArchived(conversationID, isTagged)
		}
		return nil
	})
}

func (in *intent) MuteRoom(ctx context.Context, roomID id.RoomID, until time.Time) error {
	return in.c.updateConversation(ctx, roomID, func(tx *store.Tx, conversationID int64) error {
		var untilMS int64
		if until.Equal(event.MutedForever) {
			untilMS = -1
		} else if until.After(time.Now()) {
			untilMS = until.UnixMilli()
		}
		return tx.SetConversationMutedUntil(conversationID, untilMS)
	})
}

func (in *intent) GetEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (*event.Event, error) {
	return nil, mautrix.MNotFound
}

func (c *Connector) updateConversation(ctx context.Context, roomID id.RoomID, fn func(tx *store.Tx, conversationID int64) error) error {
	return c.write(ctx, func(tx *store.Tx) error {
		conversationID, _, err := tx.ConversationByRoom(string(roomID))
		if err != nil {
			return fmt.Errorf("room %s: %w", roomID, err)
		}
		return fn(tx, conversationID)
	})
}
