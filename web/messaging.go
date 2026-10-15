package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cloud-in-a-bottle/messaging-service-spec/messaging"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
)

// MessagingPrefix is where the messaging service is served: the endpoint of the app's [[services.v2.provides]].
const MessagingPrefix = "/messaging/"

// MessagingOptions describe the network to other apps using the messaging service
// (github.com/cloud-in-a-bottle/messaging-service-spec). Set Options.Messaging to provide it.
type MessagingOptions struct {
	// The network's slug, eg "whatsapp". People's addresses are "<Network>:<remote ID>".
	Network string
	// The E.164 phone number a person's remote ID stands for, or "" if it isn't one. Nil if the network's IDs are
	// never phone numbers.
	Phone func(remoteID string) string
	// The connector's identifiers (bridge.Service.StartChat) are phone numbers, so chats can be started by phone.
	StartsChatsByPhone bool
	// Outgoing messages reach "delivered".
	DeliveryReceipts bool
}

func (o MessagingOptions) validate() error {
	if o.Network == "" || strings.ContainsAny(o.Network, ": ") {
		return errors.New("messaging options need a network slug")
	}
	return nil
}

func (s *Server) messagingRoutes() {
	routes := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		method, path, _ := strings.Cut(pattern, " ")
		routes.HandleFunc(method+" "+MessagingPrefix+"v1/"+path, h)
	}
	handle("GET account", s.svcAccount)
	handle("GET chats", s.svcChats)
	handle("POST chats", s.svcStartChat)
	handle("GET chats/{id}", s.svcChat)
	handle("GET chats/{id}/messages", s.svcMessages)
	handle("POST chats/{id}/messages", s.svcSend)
	handle("POST chats/{id}/read", s.svcRead)
	handle("POST chats/{id}/typing", s.svcTyping)
	handle("POST chats/{id}/history", s.svcLoadOlder)
	handle("GET messages", s.svcRecent)
	handle("GET messages/{id}", s.svcMessage)
	handle("PATCH messages/{id}", s.svcEdit)
	handle("DELETE messages/{id}", s.svcUnsend)
	handle("POST messages/{id}/retry", s.svcRetry)
	handle("POST messages/{id}/reactions", s.svcReact)
	handle("DELETE messages/{id}/reactions/{emoji}", s.svcUnreact)
	handle("GET search", s.svcSearch)
	handle("GET people", s.svcPeople)
	handle("GET attachments/{id}", s.attachment)
	handle("GET avatars/{sha}", s.file)
	handle("GET events", s.svcEvents)
	routes.HandleFunc(MessagingPrefix, func(w http.ResponseWriter, r *http.Request) {
		svcError(w, http.StatusNotFound, messaging.ErrNotFound, "no such endpoint")
	})
	s.mux.Handle(MessagingPrefix, messaging.RequireGrants(routes))
}

func svcError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(messaging.Error{Code: code, Detail: detail})
}

// notSupported is an action the network, or this chat, doesn't allow.
type notSupported struct{ msg string }

func (e notSupported) Error() string { return e.msg }

// rejected is an action the network would refuse, eg an edit after its window.
type rejected struct{ msg string }

func (e rejected) Error() string { return e.msg }

// svcFail is Fail with the service's error codes.
func (s *Server) svcFail(w http.ResponseWriter, r *http.Request, err error) {
	var respErr bridgev2.RespError
	var statusErr bridgev2.MessageStatus
	switch {
	case errors.Is(err, store.ErrNotFound):
		svcError(w, http.StatusNotFound, messaging.ErrNotFound, "not found")
	case errors.Is(err, store.ErrReadOnly), errors.As(err, new(notSupported)):
		svcError(w, http.StatusConflict, messaging.ErrNotSupported, err.Error())
	case errors.Is(err, bridge.ErrNotLoggedIn), errors.Is(err, bridge.ErrNoLogin):
		svcError(w, http.StatusServiceUnavailable, messaging.ErrNotConnected, err.Error())
	case errors.As(err, new(rejected)):
		svcError(w, http.StatusUnprocessableEntity, messaging.ErrRejected, err.Error())
	case errors.As(err, &respErr):
		svcError(w, http.StatusUnprocessableEntity, messaging.ErrRejected, respErr.Err)
	case errors.As(err, &statusErr):
		svcError(w, http.StatusUnprocessableEntity, messaging.ErrRejected, statusErr.Error())
	case errors.As(err, new(clientError)):
		svcError(w, http.StatusBadRequest, messaging.ErrInvalidRequest, err.Error())
	default:
		s.log.Err(err).Str("path", r.URL.Path).Msg("Messaging service request failed")
		svcError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
