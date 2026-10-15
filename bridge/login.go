package bridge

import (
	"context"
	"errors"
	"fmt"

	"maunium.net/go/mautrix/bridgev2"
)

type pendingLogin struct {
	proc bridgev2.LoginProcess
	step *bridgev2.LoginStep
}

var ErrNoLogin = errors.New("no login in progress")

func (s *Service) LoginFlows() []bridgev2.LoginFlow {
	return s.opts.Connector.GetLoginFlows()
}

// LoginStep is the step a login in progress is waiting on, or nil.
func (s *Service) LoginStep() *bridgev2.LoginStep {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.login == nil {
		return nil
	}
	return s.login.step
}

func (s *Service) StartLogin(ctx context.Context, flowID string) (*bridgev2.LoginStep, error) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.CurrentLogin(ctx) != nil {
		return nil, errors.New("already logged in")
	}
	s.cancelLoginLocked()
	user, err := s.user(ctx)
	if err != nil {
		return nil, err
	}
	proc, err := s.opts.Connector.CreateLogin(ctx, user, flowID)
	if err != nil {
		return nil, err
	}
	step, err := proc.Start(ctx)
	if err != nil {
		proc.Cancel()
		return nil, err
	}
	s.login = &pendingLogin{proc: proc}
	return s.advanceLocked(ctx, step)
}

// SubmitLogin answers the current user_input or cookies step. For a cookies step, input may instead hold a single
// "raw" entry with cookies pasted in any of the forms ParseCookies understands.
func (s *Service) SubmitLogin(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.login == nil {
		return nil, ErrNoLogin
	}
	var step *bridgev2.LoginStep
	var err error
	switch s.login.step.Type {
	case bridgev2.LoginStepTypeUserInput:
		proc, ok := s.login.proc.(bridgev2.LoginProcessUserInput)
		if !ok {
			return nil, errors.New("login doesn't take input")
		}
		step, err = proc.SubmitUserInput(ctx, input)
	case bridgev2.LoginStepTypeCookies:
		proc, ok := s.login.proc.(bridgev2.LoginProcessCookies)
		if !ok {
			return nil, errors.New("login doesn't take cookies")
		}
		if raw, ok := input["raw"]; ok {
			if input, err = ParseCookies(raw); err != nil {
				return nil, err
			}
		}
		step, err = proc.SubmitCookies(ctx, input)
	default:
		return nil, fmt.Errorf("current login step is %s, not an input step", s.login.step.Type)
	}
	if err != nil {
		return nil, err
	}
	return s.advanceLocked(ctx, step)
}

// WaitLogin waits for a display_and_wait step (eg approving the login on another device) to finish.
func (s *Service) WaitLogin(ctx context.Context) (*bridgev2.LoginStep, error) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.login == nil {
		return nil, ErrNoLogin
	}
	proc, ok := s.login.proc.(bridgev2.LoginProcessDisplayAndWait)
	if !ok || s.login.step.Type != bridgev2.LoginStepTypeDisplayAndWait {
		return nil, errors.New("current login step isn't a waiting step")
	}
	step, err := proc.Wait(ctx)
	if err != nil {
		return nil, err
	}
	return s.advanceLocked(ctx, step)
}

func (s *Service) CancelLogin() {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.cancelLoginLocked()
}

func (s *Service) cancelLoginLocked() {
	if s.login != nil {
		s.login.proc.Cancel()
		s.login = nil
	}
}

func (s *Service) advanceLocked(ctx context.Context, step *bridgev2.LoginStep) (*bridgev2.LoginStep, error) {
	if step.Type != bridgev2.LoginStepTypeComplete {
		s.login.step = step
		return step, nil
	}
	s.login = nil
	if step.CompleteParams == nil || step.CompleteParams.UserLogin == nil {
		return nil, errors.New("login completed without an account")
	}
	if err := s.recordSelf(ctx, step.CompleteParams.UserLogin); err != nil {
		return nil, err
	}
	s.broker.Publish("account", s.Account(ctx))
	return step, nil
}
