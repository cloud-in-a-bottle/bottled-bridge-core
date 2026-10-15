// Package web serves the chat UI and its JSON API. The app supplies its branding and any extra features (eg an
// importer) through Options.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"sync"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/config"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/web/static"
)

// appStaticFiles must be in Options.Static.
var appStaticFiles = []string{"favicon.svg", "apple-touch-icon.png"}

type Options struct {
	Config   config.Config
	Service  *bridge.Service
	Store    *store.Store
	Files    *files.Store
	Broker   *events.Broker
	Branding Branding
	// The app's own static files, served under /static/app/: at least favicon.svg and apple-touch-icon.png, plus the
	// modules in Branding.Extensions.
	Static fs.FS
	Log    zerolog.Logger
	// Set to provide the messaging service to other apps, under MessagingPrefix.
	Messaging *MessagingOptions
}

type Server struct {
	cfg      config.Config
	svc      *bridge.Service
	store    *store.Store
	files    *files.Store
	broker   *events.Broker
	branding Branding
	// Nil when the app doesn't provide the messaging service.
	messaging *MessagingOptions
	// Serializes sends through the messaging service, for their client IDs.
	sendMu sync.Mutex
	index  []byte
	log    zerolog.Logger
	mux    *http.ServeMux
}

func New(opts Options) (*Server, error) {
	if err := opts.Branding.validate(); err != nil {
		return nil, err
	}
	for _, name := range appStaticFiles {
		if _, err := fs.Stat(opts.Static, name); err != nil {
			return nil, fmt.Errorf("app static files: %w", err)
		}
	}
	for _, ext := range opts.Branding.Extensions {
		if _, err := fs.Stat(opts.Static, ext.Module); err != nil {
			return nil, fmt.Errorf("extension module: %w", err)
		}
	}
	if opts.Messaging != nil {
		if err := opts.Messaging.validate(); err != nil {
			return nil, err
		}
	}
	index, err := renderIndex(opts.Branding)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: opts.Config, svc: opts.Service, store: opts.Store, files: opts.Files, broker: opts.Broker,
		branding: opts.Branding, messaging: opts.Messaging, index: index, log: opts.Log, mux: http.NewServeMux(),
	}
	mux := s.mux
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, healthResponse{Status: "ok"})
	})
	mux.HandleFunc("GET /{$}", s.serveIndex)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static.FS)))
	mux.Handle("GET /static/app/", http.StripPrefix("/static/app/", http.FileServerFS(opts.Static)))

	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/login/start", s.loginStart)
	mux.HandleFunc("POST /api/login/submit", s.loginSubmit)
	mux.HandleFunc("POST /api/login/wait", s.loginWait)
	mux.HandleFunc("POST /api/login/cancel", s.loginCancel)
	mux.HandleFunc("GET /api/login/qr", s.loginQR)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("POST /api/reconnect", s.reconnect)
	mux.HandleFunc("GET /api/events", s.events)

	mux.HandleFunc("GET /api/conversations", s.conversations)
	mux.HandleFunc("POST /api/conversations", s.newConversation)
	mux.HandleFunc("GET /api/conversations/{id}", s.conversation)
	mux.HandleFunc("GET /api/conversations/{id}/messages", s.messages)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.send)
	mux.HandleFunc("POST /api/conversations/{id}/read", s.markRead)
	mux.HandleFunc("POST /api/conversations/{id}/typing", s.typing)
	mux.HandleFunc("GET /api/conversations/{id}/history", s.history)
	mux.HandleFunc("POST /api/conversations/{id}/history", s.loadOlder)
	mux.HandleFunc("POST /api/messages/{id}/reaction", s.react)
	mux.HandleFunc("POST /api/messages/{id}/edit", s.edit)
	mux.HandleFunc("DELETE /api/messages/{id}", s.unsend)
	mux.HandleFunc("POST /api/messages/{id}/retry", s.retry)
	mux.HandleFunc("POST /api/messages/{id}/discard", s.discard)
	mux.HandleFunc("POST /api/messages/{id}/viewed", s.viewed)
	mux.HandleFunc("GET /api/messages/{id}/location", s.location)
	mux.HandleFunc("GET /api/attachments/{id}", s.attachment)
	mux.HandleFunc("GET /api/files/{sha}", s.file)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/contacts", s.contacts)
	mux.HandleFunc("GET /api/people/search", s.searchPeople)
	mux.HandleFunc("GET /api/storage", s.storage)
	if s.messaging != nil {
		s.messagingRoutes()
	}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Handle adds an app route, eg for an importer. App routes should live under /api/.
func (s *Server) Handle(pattern string, handler http.HandlerFunc) {
	s.mux.HandleFunc(pattern, handler)
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(s.index)
}

type healthResponse struct {
	Status string `json:"status"`
}

type errorBody struct {
	Detail string `json:"detail"`
}

func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		panic(err)
	}
}

func writeError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Detail: detail})
}

// Fail reports err: 404 for missing rows, 409 for a missing login, 400 with the bridge's message for its errors and
// for BadRequest errors, and 500 (logged) for anything else.
func (s *Server) Fail(w http.ResponseWriter, r *http.Request, err error) {
	var respErr bridgev2.RespError
	var mxErr mautrix.RespError
	var statusErr bridgev2.MessageStatus
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrReadOnly):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, bridge.ErrNoLogin):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, bridge.ErrNotViewOnce):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &respErr):
		writeError(w, http.StatusBadRequest, respErr.Err)
	case errors.As(err, &mxErr):
		writeError(w, http.StatusBadRequest, mxErr.Err)
	case errors.As(err, &statusErr):
		writeError(w, http.StatusBadRequest, statusErr.Error())
	case errors.As(err, new(clientError)):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Err(err).Str("path", r.URL.Path).Msg("Request failed")
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

type clientError struct{ msg string }

func (e clientError) Error() string { return e.msg }

// BadRequest is an error Fail shows to the user as it is, as a problem with what they asked for.
func BadRequest(msg string) error { return clientError{msg} }

// ReadJSON decodes a JSON request body of at most 1MB.
func ReadJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v); err != nil {
		return BadRequest("invalid JSON body: " + err.Error())
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, BadRequest("invalid id")
	}
	return id, nil
}

// QueryInt reads an integer query parameter, or fallback if it's absent.
func QueryInt(r *http.Request, name string, fallback int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, BadRequest("invalid " + name)
	}
	return v, nil
}
