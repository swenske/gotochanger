package api

import (
	"net/http"
)

func (s *Server) handleListCleaningTapes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.lib.CleaningTapes())
}

func (s *Server) handleCreateCleaningTape(w http.ResponseWriter, r *http.Request) {
	vol, err := s.lib.CreateCleaningTape()
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, vol)
}
