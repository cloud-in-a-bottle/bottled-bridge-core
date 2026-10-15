package store

import (
	"context"
	"os"
	"sort"
	"strings"
)

type CategoryUsage struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes"`
}

type ConversationUsage struct {
	ConversationID int64  `json:"conversation_id"`
	Name           string `json:"name"`
	Messages       int64  `json:"messages"`
	Attachments    int64  `json:"attachments"`
	Bytes          int64  `json:"bytes"`
}

type LargeAttachment struct {
	AttachmentID     int64  `json:"attachment_id"`
	MessageID        int64  `json:"message_id"`
	ConversationID   int64  `json:"conversation_id"`
	ConversationName string `json:"conversation_name"`
	ContentType      string `json:"content_type"`
	Filename         string `json:"filename"`
	Bytes            int64  `json:"bytes"`
	SentAt           int64  `json:"sent_at"`
}

type StorageReport struct {
	Messages           int64 `json:"messages"`
	Conversations      int64 `json:"conversations"`
	Attachments        int64 `json:"attachments"`
	MissingAttachments int64 `json:"missing_attachments"`
	// On-disk sizes. Attachments are deduplicated by content, so AttachmentBytes counts each file once.
	AttachmentBytes   int64               `json:"attachment_bytes"`
	DatabaseBytes     int64               `json:"database_bytes"`
	BridgeBytes       int64               `json:"bridge_bytes"`
	Categories        []CategoryUsage     `json:"categories"`
	TopConversations  []ConversationUsage `json:"top_conversations"`
	LargestAttachment []LargeAttachment   `json:"largest_attachments"`
}

// Fixed order: it is also the order of the chart's color slots, so a category keeps its color.
var categories = [][2]string{
	{"images", "Images & stickers"},
	{"videos", "Videos"},
	{"audio", "Audio & voice notes"},
	{"files", "Other files"},
	{"text", "Message text"},
}

func category(msgtype, contentType string) string {
	switch {
	case msgtype == "m.audio" || strings.HasPrefix(contentType, "audio/"):
		return "audio"
	case msgtype == "m.image" || msgtype == "m.sticker" || strings.HasPrefix(contentType, "image/"):
		return "images"
	case msgtype == "m.video" || strings.HasPrefix(contentType, "video/"):
		return "videos"
	}
	return "files"
}

// SQLiteFileBytes is the size of a sqlite database including its WAL.
func SQLiteFileBytes(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if info, err := os.Stat(p); err == nil {
			total += info.Size()
		}
	}
	return total
}

// StorageReport measures the store. fileSize gives a stored file's size on disk (-1 if missing); sizes come from
// the files rather than the attachment metadata, which can be missing or wrong.
func (s *Store) StorageReport(ctx context.Context, fileSize func(sha string) int64, dbPath, bridgeDBPath string) (StorageReport, error) {
	r := StorageReport{DatabaseBytes: SQLiteFileBytes(dbPath), BridgeBytes: SQLiteFileBytes(bridgeDBPath)}
	type attachmentRow struct {
		id, messageID, conversationID, sentAt int64
		sha, msgtype, contentType, filename   string
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.message_id, m.conversation_id, m.sent_at, coalesce(a.file_sha256, ''), a.msgtype,
			coalesce(a.content_type, ''), coalesce(a.filename, '')
		FROM attachments a JOIN messages m ON m.id = a.message_id ORDER BY m.sent_at`)
	if err != nil {
		return r, err
	}
	var attachments []attachmentRow
	for rows.Next() {
		var a attachmentRow
		if err = rows.Scan(&a.id, &a.messageID, &a.conversationID, &a.sentAt, &a.sha, &a.msgtype, &a.contentType, &a.filename); err != nil {
			_ = rows.Close()
			return r, err
		}
		attachments = append(attachments, a)
	}
	if err = rows.Close(); err != nil {
		return r, err
	}

	counts := map[string]int64{}
	sizes := map[string]int64{}
	fileSizes := map[string]int64{}
	firstUse := map[string]attachmentRow{}
	conversationBytes := map[int64]int64{}
	conversationSeen := map[[2]any]bool{}
	for _, a := range attachments {
		r.Attachments++
		if a.sha == "" {
			r.MissingAttachments++
			continue
		}
		size, known := fileSizes[a.sha]
		if !known {
			size = fileSize(a.sha)
			fileSizes[a.sha] = size
		}
		if size < 0 {
			r.MissingAttachments++
			continue
		}
		cat := category(a.msgtype, a.contentType)
		counts[cat]++
		if _, seen := firstUse[a.sha]; !seen {
			firstUse[a.sha] = a
			sizes[cat] += size
			r.AttachmentBytes += size
		}
		key := [2]any{a.conversationID, a.sha}
		if !conversationSeen[key] {
			conversationSeen[key] = true
			conversationBytes[a.conversationID] += size
		}
	}
	var textCount, textBytes int64
	if err = s.db.QueryRowContext(ctx,
		"SELECT count(*), coalesce(sum(length(CAST(body AS BLOB))), 0) FROM messages WHERE body IS NOT NULL",
	).Scan(&textCount, &textBytes); err != nil {
		return r, err
	}
	counts["text"], sizes["text"] = textCount, textBytes
	for _, c := range categories {
		r.Categories = append(r.Categories, CategoryUsage{Key: c[0], Label: c[1], Count: counts[c[0]], Bytes: sizes[c[0]]})
	}

	names := map[int64]string{}
	rows, err = s.db.QueryContext(ctx, `
		SELECT c.id, `+conversationNameSQL+`,
			(SELECT count(*) FROM messages m WHERE m.conversation_id = c.id),
			(SELECT count(*) FROM attachments a JOIN messages m ON m.id = a.message_id WHERE m.conversation_id = c.id)
		FROM conversations c LEFT JOIN people op ON op.id = c.other_person_id`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var u ConversationUsage
		if err = rows.Scan(&u.ConversationID, &u.Name, &u.Messages, &u.Attachments); err != nil {
			_ = rows.Close()
			return r, err
		}
		names[u.ConversationID] = u.Name
		if u.Messages > 0 {
			r.Conversations++
			r.Messages += u.Messages
			u.Bytes = conversationBytes[u.ConversationID]
			r.TopConversations = append(r.TopConversations, u)
		}
	}
	if err = rows.Close(); err != nil {
		return r, err
	}
	sort.Slice(r.TopConversations, func(i, j int) bool {
		a, b := r.TopConversations[i], r.TopConversations[j]
		if a.Bytes != b.Bytes {
			return a.Bytes > b.Bytes
		}
		return a.Messages > b.Messages
	})
	r.TopConversations = r.TopConversations[:min(10, len(r.TopConversations))]

	shas := make([]string, 0, len(firstUse))
	for sha := range firstUse {
		shas = append(shas, sha)
	}
	sort.Slice(shas, func(i, j int) bool { return fileSizes[shas[i]] > fileSizes[shas[j]] })
	r.LargestAttachment = []LargeAttachment{}
	for _, sha := range shas[:min(20, len(shas))] {
		a := firstUse[sha]
		r.LargestAttachment = append(r.LargestAttachment, LargeAttachment{
			AttachmentID:     a.id,
			MessageID:        a.messageID,
			ConversationID:   a.conversationID,
			ConversationName: names[a.conversationID],
			ContentType:      a.contentType,
			Filename:         a.filename,
			Bytes:            fileSizes[sha],
			SentAt:           a.sentAt,
		})
	}
	return r, nil
}
