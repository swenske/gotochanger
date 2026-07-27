package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const sessionCookieName = "gotochanger_session"

// sessionTTL is how long a browser session stays valid without a fresh
// login. Sessions live only in memory: a daemon restart requires signing
// in again, which is an acceptable trade-off for a lab/staging tool and
// avoids persisting session secrets to disk.
const sessionTTL = 12 * time.Hour

type session struct {
	Username  string
	ExpiresAt time.Time
}

// SessionStore is an in-memory, mutex-protected store of active browser
// sessions. Only the username is stored; the caller's current role is
// always resolved live from the UserStore so a role change or account
// deletion takes effect immediately, even for already-logged-in sessions.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]*session{}}
}

// Create starts a new session for username and returns its opaque ID.
func (s *SessionStore) Create(username string) (string, error) {
	id, err := randomID(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.sessions[id] = &session{Username: username, ExpiresAt: time.Now().Add(sessionTTL)}
	return id, nil
}

// Username returns the username bound to a still-valid session ID.
func (s *SessionStore) Username(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || time.Now().After(sess.ExpiresAt) {
		delete(s.sessions, id)
		return "", false
	}
	return sess.Username, true
}

// Delete invalidates a session (logout).
func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// DeleteForUser invalidates every session belonging to username, used when
// an account is deleted or its password is forcibly reset.
func (s *SessionStore) DeleteForUser(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.Username == username {
			delete(s.sessions, id)
		}
	}
}

func (s *SessionStore) sweepLocked() {
	now := time.Now()
	for id, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}

func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}
