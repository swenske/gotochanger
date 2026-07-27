package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/swenske/gotochanger/internal/library"
)

// authStateResponse tells the web UI what to render: the login form, the
// initial-admin-password bootstrap form, or (once authenticated) the
// dashboard, including whether the account must change its password first.
type authStateResponse struct {
	BootstrapRequired  bool   `json:"bootstrap_required"`
	Authenticated      bool   `json:"authenticated"`
	Username           string `json:"username,omitempty"`
	Role               Role   `json:"role,omitempty"`
	MustChangePassword bool   `json:"must_change_password,omitempty"`
	Version            string `json:"version,omitempty"`
}

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	resp := authStateResponse{BootstrapRequired: s.users.BootstrapRequired(), Version: s.version}
	if p, ok := s.principalFromSession(r); ok {
		resp.Authenticated = true
		resp.Username = p.Subject
		resp.Role = p.Role
		if u, ok := s.users.Get(p.Subject); ok {
			resp.MustChangePassword = u.MustChangePassword
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type bootstrapRequest struct {
	Password        string `json:"password"`
	ConfirmPassword string `json:"confirm_password"`
}

func (s *Server) handleAuthBootstrap(w http.ResponseWriter, r *http.Request) {
	var req bootstrapRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeAuthBootstrapFailure, "failed to bootstrap admin account", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Password != req.ConfirmPassword {
		err := fmt.Errorf("password and confirmation do not match")
		s.emitFailure(r, library.EventCodeAuthBootstrapFailure, "failed to bootstrap admin account", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.users.SetInitialAdminPassword(req.Password); err != nil {
		s.emitFailure(r, library.EventCodeAuthBootstrapFailure, "failed to bootstrap admin account", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeAuthBootstrapSuccess, Message: "bootstrapped admin account", Actor: DefaultAdminUsername, Source: "session"})
	s.startSession(w, r, DefaultAdminUsername, RoleAdmin, false)
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeAuthLoginFailure, "failed login attempt", err, map[string]string{"username": req.Username})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	user, err := s.users.Authenticate(req.Username, req.Password)
	if err != nil {
		s.emitFailure(r, library.EventCodeAuthLoginFailure, "failed login attempt", err, map[string]string{"username": req.Username})
		switch {
		case errors.Is(err, ErrBootstrapRequired):
			writeError(w, http.StatusConflict, err)
		case errors.Is(err, ErrAccountLocked):
			writeError(w, http.StatusTooManyRequests, err)
		default:
			writeError(w, http.StatusUnauthorized, ErrInvalidCredentials)
		}
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeAuthLoginSuccess, Message: "user logged in", Actor: user.Username, Source: "session"})
	s.startSession(w, r, user.Username, user.Role, user.MustChangePassword)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, username string, role Role, mustChange bool) {
	id, err := s.sessions.Create(username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	setSessionCookie(w, r, id)
	writeJSON(w, http.StatusOK, authStateResponse{Authenticated: true, Username: username, Role: role, MustChangePassword: mustChange, Version: s.version})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	actor := ""
	if p, ok := principalFrom(r); ok {
		actor = p.Subject
	}
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.Delete(c.Value)
	}
	clearSessionCookie(w)
	s.emitEvent(r, library.Event{Code: library.EventCodeAuthLogoutSuccess, Message: "user logged out", Actor: actor})
	w.WriteHeader(http.StatusNoContent)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r)
	if !ok {
		err := fmt.Errorf("authentication required")
		s.emitFailure(r, library.EventCodeAuthChangePasswordFailure, "failed password change", err, nil)
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeAuthChangePasswordFailure, "failed password change", err, map[string]string{"username": p.Subject})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.users.ChangePassword(p.Subject, req.CurrentPassword, req.NewPassword); err != nil {
		s.emitFailure(r, library.EventCodeAuthChangePasswordFailure, "failed password change", err, map[string]string{"username": p.Subject})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeAuthChangePasswordSuccess, Message: "password changed", Actor: p.Subject})
	w.WriteHeader(http.StatusNoContent)
}
