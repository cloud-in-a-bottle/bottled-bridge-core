package bridge

import (
	"slices"
	"testing"

	"github.com/cloud-in-a-bottle/messaging-service-spec/messaging"
)

// As mautrix-whatsapp reports a DM's features.
const whatsAppDMFeatures = `{"delete":2,"delete_chat":true,"delete_max_age":172800,"edit":2,"edit_max_age":900,
"file":{"m.audio":{"caption":-1,"max_size":2097152000,"mime_types":{"audio/mpeg":2}},
"m.file":{"caption":2,"max_size":2097152000,"mime_types":{"*/*":2}},
"m.image":{"caption":2,"max_size":2097152000,"mime_types":{"image/gif":-2,"image/jpeg":2}},
"m.sticker":{"caption":-1,"max_size":2097152000,"mime_types":{"image/webp":2}},
"m.video":{"caption":2,"max_size":2097152000,"mime_types":{"video/mp4":2}}},
"max_text_length":65536,"reaction":2,"reaction_count":1,"read_receipts":true,"reply":2,"typing_notifications":true}`

func TestCapabilitiesFromRoomFeatures(t *testing.T) {
	s := &Service{opts: Options{MaxFileSize: 512 << 20}}
	caps, err := s.capabilitiesFrom(whatsAppDMFeatures)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Reply || !caps.Typing || !caps.ReadReceipts || caps.MaxTextLength != 65536 {
		t.Fatalf("unexpected flags: %+v", caps)
	}
	if caps.Edit == nil || *caps.Edit.MaxAgeS != 900 || caps.Edit.MaxCount != nil {
		t.Fatalf("unexpected edit: %+v", caps.Edit)
	}
	if caps.Unsend == nil || *caps.Unsend.MaxAgeS != 172800 {
		t.Fatalf("unexpected unsend: %+v", caps.Unsend)
	}
	if caps.Reactions == nil || caps.Reactions.MaxPerPerson != 1 || caps.Reactions.Emoji != nil {
		t.Fatalf("unexpected reactions: %+v", caps.Reactions)
	}
	a := caps.Attachments
	want := []messaging.AttachmentKind{messaging.KindImage, messaging.KindVideo, messaging.KindAudio, messaging.KindFile}
	if a == nil || !slices.Equal(a.Kinds, want) || !a.Caption || a.MaxSizeBytes != 512<<20 || a.MaxPerMessage != 1 {
		t.Fatalf("unexpected attachments: %+v", a)
	}

	none, err := s.capabilitiesFrom(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	if none.Reply || none.Edit != nil || none.Unsend != nil || none.Reactions != nil || none.Attachments != nil {
		t.Fatalf("expected nothing allowed: %+v", none)
	}
}
