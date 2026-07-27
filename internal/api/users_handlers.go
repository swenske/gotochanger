package api

import (
	"errors"
	"net/http"

	"github.com/swenske/gotochanger/internal/library"
)

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.users.List())
}

type createUserRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeConfigUserCreateFailure, "failed to create user", err, nil)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	role, err := ParseRole(req.Role)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigUserCreateFailure, "failed to create user", err, map[string]string{"username": req.Username})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	user, err := s.users.CreateUser(req.Username, role, req.Password)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigUserCreateFailure, "failed to create user", err, map[string]string{"username": req.Username, "role": string(role)})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigUserCreateSuccess, Message: "created user", Detail: map[string]string{"username": user.Username, "role": string(user.Role)}})
	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if err := s.users.DeleteUser(username); err != nil {
		s.emitFailure(r, library.EventCodeConfigUserDeleteFailure, "failed to delete user", err, map[string]string{"username": username})
		writeError(w, statusForUserErr(err), err)
		return
	}
	s.sessions.DeleteForUser(username)
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigUserDeleteSuccess, Message: "deleted user", Detail: map[string]string{"username": username}})
	w.WriteHeader(http.StatusNoContent)
}

type setRoleRequest struct {
	Role string `json:"role"`
}

func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	var req setRoleRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeConfigUserRoleSetFailure, "failed to set user role", err, map[string]string{"username": username})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	role, err := ParseRole(req.Role)
	if err != nil {
		s.emitFailure(r, library.EventCodeConfigUserRoleSetFailure, "failed to set user role", err, map[string]string{"username": username, "role": req.Role})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.users.SetRole(username, role); err != nil {
		s.emitFailure(r, library.EventCodeConfigUserRoleSetFailure, "failed to set user role", err, map[string]string{"username": username, "role": string(role)})
		writeError(w, statusForUserErr(err), err)
		return
	}
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigUserRoleSetSuccess, Message: "updated user role", Detail: map[string]string{"username": username, "role": string(role)}})
	w.WriteHeader(http.StatusNoContent)
}

type resetPasswordRequest struct {
	NewPassword string `json:"new_password"`
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	var req resetPasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		s.emitFailure(r, library.EventCodeConfigUserPasswordResetFailure, "failed to reset user password", err, map[string]string{"username": username})
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.users.AdminResetPassword(username, req.NewPassword); err != nil {
		s.emitFailure(r, library.EventCodeConfigUserPasswordResetFailure, "failed to reset user password", err, map[string]string{"username": username})
		writeError(w, statusForUserErr(err), err)
		return
	}
	s.sessions.DeleteForUser(username)
	s.emitEvent(r, library.Event{Code: library.EventCodeConfigUserPasswordResetSuccess, Message: "reset user password", Detail: map[string]string{"username": username}})
	w.WriteHeader(http.StatusNoContent)
}

func statusForUserErr(err error) int {
	switch {
	case errors.Is(err, errUserNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrLastAdmin):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}
