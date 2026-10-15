package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/skip2/go-qrcode"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
)

type stateResponse struct {
	Account    bridge.Account       `json:"account"`
	LoginFlows []bridgev2.LoginFlow `json:"login_flows"`
	LoginStep  *bridgev2.LoginStep  `json:"login_step"`
	// Whether "New chat" can search the network.
	CanSearchPeople bool `json:"can_search_people"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, stateResponse{
		Account:         s.svc.Account(r.Context()),
		LoginFlows:      s.svc.LoginFlows(),
		LoginStep:       s.svc.LoginStep(),
		CanSearchPeople: s.svc.CanSearchPeople(r.Context()),
	})
}

// loginQR renders the QR code of the current login step (eg WhatsApp's device linking) as a PNG.
func (s *Server) loginQR(w http.ResponseWriter, r *http.Request) {
	step := s.svc.LoginStep()
	if step == nil || step.Type != bridgev2.LoginStepTypeDisplayAndWait || step.DisplayAndWaitParams == nil ||
		step.DisplayAndWaitParams.Type != bridgev2.LoginDisplayTypeQR {
		writeError(w, http.StatusNotFound, "no QR code to show")
		return
	}
	png, err := qrcode.Encode(step.DisplayAndWaitParams.Data, qrcode.Medium, 512)
	if err != nil {
		s.Fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *Server) loginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FlowID string `json:"flow_id"`
	}
	if err := ReadJSON(r, &req); err != nil {
		s.Fail(w, r, err)
		return
	}
	step, err := s.svc.StartLogin(r.Context(), req.FlowID)
	if err != nil {
		s.Fail(w, r, loginError(err))
		return
	}
	WriteJSON(w, step)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fields map[string]string `json:"fields"`
	}
	if err := ReadJSON(r, &req); err != nil {
		s.Fail(w, r, err)
		return
	}
	step, err := s.svc.SubmitLogin(r.Context(), req.Fields)
	if err != nil {
		s.Fail(w, r, loginError(err))
		return
	}
	WriteJSON(w, step)
}

func (s *Server) loginWait(w http.ResponseWriter, r *http.Request) {
	step, err := s.svc.WaitLogin(r.Context())
	if err != nil {
		s.Fail(w, r, loginError(err))
		return
	}
	WriteJSON(w, step)
}

func (s *Server) loginCancel(w http.ResponseWriter, r *http.Request) {
	s.svc.CancelLogin()
	w.WriteHeader(http.StatusNoContent)
}

// loginError shows the network's reasons for refusing a login to the user rather than treating them as server
// errors.
func loginError(err error) error {
	if err == bridge.ErrNoLogin {
		return err
	}
	return BadRequest(fmt.Sprintf("Login failed: %s", err))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Logout(r.Context()); err != nil {
		s.Fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Reconnect(r.Context()); err != nil {
		s.Fail(w, r, BadRequest(err.Error()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := s.broker.Subscribe()
	defer unsubscribe()
	_, _ = fmt.Fprint(w, "event: hello\ndata: {}\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
		case evt := <-ch:
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Kind, evt.Data)
		}
		flusher.Flush()
	}
}
