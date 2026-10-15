package localmx

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"

	"go.mau.fi/util/random"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
)

// ServerName is the made-up homeserver all of the bridge's Matrix IDs live on. Nothing ever resolves it. Room IDs
// with it are stored in both databases, so it stays "fb.local" (from when this was a Messenger client) for every
// network.
const ServerName = "fb.local"

const (
	OwnerMXID   = id.UserID("@owner:" + ServerName)
	BotMXID     = id.UserID("@bot:" + ServerName)
	ghostPrefix = "@fb_"
	mxcPrefix   = "mxc://" + ServerName + "/"
)

func FormatGhostMXID(userID networkid.UserID) id.UserID {
	return id.UserID(ghostPrefix + string(userID) + ":" + ServerName)
}

func ParseGhostMXID(userID id.UserID) (networkid.UserID, bool) {
	s := string(userID)
	if !strings.HasPrefix(s, ghostPrefix) || !strings.HasSuffix(s, ":"+ServerName) {
		return "", false
	}
	return networkid.UserID(strings.TrimSuffix(strings.TrimPrefix(s, ghostPrefix), ":"+ServerName)), true
}

var plainID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func idPart(s string) string {
	if plainID.MatchString(s) {
		return s
	}
	return "h" + hashID(s)[:16]
}

// RoomIDFor is deterministic so that a chat the bridge deletes and recreates maps back to the same conversation.
func RoomIDFor(key networkid.PortalKey) id.RoomID {
	return id.RoomID("!" + idPart(string(key.ID)) + "." + idPart(string(key.Receiver)) + ":" + ServerName)
}

func hashID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// MessageEventID is deterministic so that a message delivered twice (live and by backfill, or again after a
// restart) is stored once, and so replies to messages that haven't been backfilled yet still resolve later.
func MessageEventID(key networkid.PortalKey, messageID networkid.MessageID, partID networkid.PartID) id.EventID {
	return id.EventID("$m" + hashID(string(key.ID), string(key.Receiver), string(messageID), string(partID))[:40])
}

func ReactionEventID(key networkid.PortalKey, messageID networkid.MessageID, partID networkid.PartID, sender networkid.UserID, emoji string) id.EventID {
	return id.EventID("$r" + hashID(string(key.ID), string(key.Receiver), string(messageID), string(partID), string(sender), emoji)[:40])
}

func RandomEventID() id.EventID {
	return id.EventID("$x" + random.String(40))
}

func ContentURI(sha string) id.ContentURIString {
	return id.ContentURIString(mxcPrefix + sha)
}

// SHAFromContentURI returns "" for URIs that don't point into the local file store.
func SHAFromContentURI(uri id.ContentURIString) string {
	sha, ok := strings.CutPrefix(string(uri), mxcPrefix)
	if !ok || !files.ValidSHA(sha) {
		return ""
	}
	return sha
}
