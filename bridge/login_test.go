package bridge_test

import (
	"errors"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridgetest"
)

// While a waiting step is being waited on (eg a QR code nobody has scanned yet), the step can still be read (to show
// the code) and the login cancelled.
func TestWaitingLoginCanBeShownAndCancelled(t *testing.T) {
	h := bridgetest.New(t)
	if err := h.Svc.Logout(h.Ctx); err != nil {
		t.Fatal(err)
	}
	step, err := h.Svc.StartLogin(h.Ctx, "qr")
	if err != nil {
		t.Fatal(err)
	}
	if step.Type != bridgev2.LoginStepTypeDisplayAndWait || step.DisplayAndWaitParams.Data != "fake-qr" {
		t.Fatalf("unexpected step: %+v", step)
	}
	waited := make(chan error, 1)
	go func() {
		_, err := h.Svc.WaitLogin(h.Ctx)
		waited <- err
	}()
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		if current := h.Svc.LoginStep(); current == nil || current.DisplayAndWaitParams.Data != "fake-qr" {
			t.Errorf("unexpected current step: %+v", current)
		}
		h.Svc.CancelLogin()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reading or cancelling the login blocked on the wait")
	}
	select {
	case err = <-waited:
		if !errors.Is(err, bridge.ErrNoLogin) {
			t.Fatalf("expected the wait to end with ErrNoLogin, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait didn't end on cancel")
	}
}
