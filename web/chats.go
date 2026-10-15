package web

import (
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/webp"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

const maxUploadBytes = 100 << 20

// idResponse names a created conversation or message.
type idResponse struct {
	ID int64 `json:"id"`
}

func (s *Server) conversations(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListConversations(r.Context())
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, list)
}

// newConversation opens the DM with a person: one this server knows (person_id), one found by searching (remote_id),
// or one the network can look up by what the user typed (identifier, eg a phone number).
func (s *Server) newConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PersonID   int64  `json:"person_id"`
		RemoteID   string `json:"remote_id"`
		Identifier string `json:"identifier"`
	}
	if err := ReadJSON(r, &req); err != nil {
		s.Fail(w, r, err)
		return
	}
	target := req.RemoteID
	switch {
	case req.Identifier != "":
		if s.branding.IdentifierLabel == "" {
			s.Fail(w, r, BadRequest("this network can't start chats by identifier"))
			return
		}
		target = strings.TrimSpace(req.Identifier)
	case target == "":
		var err error
		if target, err = s.store.PersonRemoteID(r.Context(), req.PersonID); err != nil {
			s.Fail(w, r, err)
			return
		}
		fallthrough
	default:
		// People imported by name only have IDs like "name:Alice": there's nobody on the network to message.
		if !store.IsNetworkPerson(target) {
			s.Fail(w, r, BadRequest("not someone on "+s.branding.Network))
			return
		}
	}
	conversationID, err := s.svc.StartChat(r.Context(), target)
	if errors.Is(err, bridge.ErrNotLoggedIn) {
		err = BadRequest(err.Error())
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, idResponse{ID: conversationID})
}

func (s *Server) searchPeople(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		s.Fail(w, r, BadRequest("q is required"))
		return
	}
	found, err := s.svc.SearchPeople(r.Context(), q)
	if errors.Is(err, bridge.ErrNotLoggedIn) || errors.Is(err, bridge.ErrNoSearch) {
		err = BadRequest(err.Error())
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, found)
}

func (s *Server) conversation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	detail, err := s.store.ConversationDetail(r.Context(), id)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, detail)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	before, err := QueryInt(r, "before", 0)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	limit, err := QueryInt(r, "limit", 50)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	page, err := s.store.MessagesPage(r.Context(), id, before, int(min(max(limit, 1), 500)))
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, page)
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err = r.ParseMultipartForm(32 << 20); err != nil {
		s.Fail(w, r, BadRequest("invalid form: "+err.Error()))
		return
	}
	defer r.MultipartForm.RemoveAll()
	var replyTo int64
	if raw := r.FormValue("quote_id"); raw != "" {
		if _, err = fmt.Sscan(raw, &replyTo); err != nil {
			s.Fail(w, r, BadRequest("invalid quote_id"))
			return
		}
	}
	var outgoing []bridge.OutgoingFile
	for _, header := range r.MultipartForm.File["files"] {
		f, err := s.storeUpload(header.Filename, header.Header.Get("Content-Type"), func() (io.ReadSeekCloser, error) {
			return header.Open()
		})
		if err != nil {
			s.Fail(w, r, err)
			return
		}
		outgoing = append(outgoing, f)
	}
	messageID, err := s.svc.Send(r.Context(), id, strings.TrimSpace(r.FormValue("body")), replyTo, outgoing)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, idResponse{ID: messageID})
}

func (s *Server) storeUpload(name, contentType string, open func() (io.ReadSeekCloser, error)) (bridge.OutgoingFile, error) {
	out := bridge.OutgoingFile{Filename: filepath.Base(name)}
	if out.Filename == "." || out.Filename == "/" {
		out.Filename = "file"
	}
	src, err := open()
	if err != nil {
		return out, err
	}
	defer src.Close()
	if out.SHA256, err = s.files.PutStream(func(w io.Writer) error {
		_, err := io.Copy(w, src)
		return err
	}); err != nil {
		return out, err
	}
	out.Size = s.files.Size(out.SHA256)
	out.ContentType = contentType
	if out.ContentType == "" || out.ContentType == "application/octet-stream" {
		out.ContentType = mime.TypeByExtension(filepath.Ext(out.Filename))
	}
	if out.ContentType == "" {
		head := make([]byte, 512)
		if _, err = src.Seek(0, io.SeekStart); err != nil {
			return out, err
		}
		n, _ := io.ReadFull(src, head)
		out.ContentType = http.DetectContentType(head[:n])
	}
	if strings.HasPrefix(out.ContentType, "image/") {
		if f, err := os.Open(s.files.Path(out.SHA256)); err == nil {
			if cfg, _, err := image.DecodeConfig(f); err == nil {
				out.Width, out.Height = cfg.Width, cfg.Height
			}
			_ = f.Close()
		}
	}
	return out, nil
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.svc.MarkRead(r.Context(), id)
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) typing(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	var req struct {
		Stop bool `json:"stop"`
	}
	if err == nil {
		err = ReadJSON(r, &req)
	}
	if err == nil {
		err = s.svc.SetTyping(r.Context(), id, !req.Stop)
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) react(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	var req struct {
		Emoji *string `json:"emoji"`
	}
	if err == nil {
		err = ReadJSON(r, &req)
	}
	if err == nil {
		emoji := ""
		if req.Emoji != nil {
			emoji = *req.Emoji
		}
		err = s.svc.React(r.Context(), id, emoji)
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	var req struct {
		Body string `json:"body"`
	}
	if err == nil {
		err = ReadJSON(r, &req)
	}
	if err == nil && strings.TrimSpace(req.Body) == "" {
		err = BadRequest("body is required")
	}
	if err == nil {
		err = s.svc.Edit(r.Context(), id, strings.TrimSpace(req.Body))
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unsend(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.svc.Unsend(r.Context(), id)
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	newID, err := s.svc.Retry(r.Context(), id)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, idResponse{ID: newID})
}

func (s *Server) discard(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.svc.Discard(r.Context(), id)
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) location(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	loc, err := s.store.MessageLocation(r.Context(), id)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, loc)
}

// inlineSafe lists the types shown in the page. Anything else is served as a download: an HTML or SVG file someone
// sent would otherwise run scripts on this app's origin.
func inlineSafe(contentType string) bool {
	switch {
	case contentType == "image/svg+xml":
		return false
	case strings.HasPrefix(contentType, "image/"), strings.HasPrefix(contentType, "video/"),
		strings.HasPrefix(contentType, "audio/"), contentType == "application/pdf":
		return true
	}
	return false
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, sha, contentType, filename string) {
	f, err := os.Open(s.files.Path(sha))
	if err != nil {
		s.Fail(w, r, store.ErrNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	disposition := "inline"
	if !inlineSafe(contentType) {
		disposition = "attachment"
		contentType = "application/octet-stream"
	}
	if filename == "" {
		filename = sha
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	// Content-addressed, so it never changes.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	f, err := s.store.AttachmentFile(r.Context(), id)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	s.serveFile(w, r, f.SHA256, f.ContentType, f.Filename)
}

// file serves avatars, which are referred to by hash.
func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !files.ValidSHA(sha) {
		s.Fail(w, r, store.ErrNotFound)
		return
	}
	f, err := os.Open(s.files.Path(sha))
	if err != nil {
		s.Fail(w, r, store.ErrNotFound)
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	_ = f.Close()
	s.serveFile(w, r, sha, http.DetectContentType(head[:n]), "")
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	hits, err := s.store.Search(r.Context(), r.URL.Query().Get("q"), 100)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, hits)
}

func (s *Server) contacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := s.store.Contacts(r.Context())
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, contacts)
}

func (s *Server) storage(w http.ResponseWriter, r *http.Request) {
	report, err := s.store.StorageReport(r.Context(), s.files.Size, s.cfg.DBPath, s.cfg.BridgeDBPath)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, report)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	state, err := s.svc.History(r.Context(), id)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	WriteJSON(w, state)
}

// loadOlder starts fetching a page of older messages from the network. The result arrives as a "history" event.
func (s *Server) loadOlder(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	if !s.branding.OnDemandHistory {
		s.Fail(w, r, BadRequest("this app doesn't fetch older history on demand"))
		return
	}
	err = s.svc.LoadOlder(r.Context(), id)
	if errors.Is(err, bridge.ErrNotLoggedIn) || errors.Is(err, bridge.ErrHistoryBusy) {
		err = BadRequest(err.Error())
	}
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
