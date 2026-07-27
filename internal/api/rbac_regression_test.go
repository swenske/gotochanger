package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// wizardRoutes and their methods - every one of these was registered with no
// role requirement at all before this, on a "public for initial setup"
// rationale that nothing ever revoked once setup finished.
var wizardRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/wizard"},
	{http.MethodPost, "/api/v1/wizard"},
	{http.MethodPost, "/api/v1/wizard/complete"},
	{http.MethodPost, "/api/v1/wizard/reset"},
	{http.MethodGet, "/api/v1/wizard/options"},
}

// TestWizardRoutesRejectAnonymous is the security regression: an
// unauthenticated caller that can reach the TCP listener must not be able to
// read the library topology, rewrite it, or reset the wizard.
func TestWizardRoutesRejectAnonymous(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.PublicHandler()

	for _, rt := range wizardRoutes {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, reqJSON(t, rt.method, rt.path, map[string]any{}))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected %d for an anonymous caller, got %d body=%s",
				rt.method, rt.path, http.StatusUnauthorized, rr.Code, rr.Body.String())
		}
	}
}

// TestWizardRoutesRejectNonAdmin covers the other half: an authenticated but
// non-admin principal is still not allowed to rewrite topology, matching
// every other topology-writing endpoint (magazines, drives, logical
// libraries), which have always been Admin-only.
func TestWizardRoutesRejectNonAdmin(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.PublicHandler()

	token, err := s.tokens.Add("viewer-token", RoleViewer)
	if err != nil {
		t.Fatalf("add viewer token: %v", err)
	}
	for _, rt := range wizardRoutes {
		rr := httptest.NewRecorder()
		req := reqJSON(t, rt.method, rt.path, map[string]any{})
		req.Header.Set("X-Api-Key", token)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s %s: expected %d for a viewer, got %d body=%s",
				rt.method, rt.path, http.StatusForbidden, rr.Code, rr.Body.String())
		}
	}
}

// TestWizardRoutesAllowAdminSession confirms the fix didn't break the real
// UI flow: the web UI reaches the wizard only from enterAppOrWizard, after
// bootstrap or login has established an Admin session.
func TestWizardRoutesAllowAdminSession(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.PublicHandler()
	cookie := loginCookie(t, h, DefaultAdminUsername, "AdminPass123!")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wizard", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/wizard as admin: expected %d got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
}

// TestWizardRoutesAllowTrustedSocket confirms gotochangerctl and the Bareos
// shim are unaffected - the trusted Unix socket presents every request as
// admin, so it never consults roles at all.
func TestWizardRoutesAllowTrustedSocket(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.TrustedHandler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/wizard", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/wizard over trusted socket: expected %d got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
}

// TestBackupDownloadIsAdminOnly is the credential-exposure regression: a
// backup is a VACUUM INTO of the whole state.db, which has held the users
// and tokens tables ever since auth moved out of users.json/tokens.json - so
// an Operator-downloadable backup handed out every admin's password hash.
func TestBackupDownloadIsAdminOnly(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.PublicHandler()

	operatorToken, err := s.tokens.Add("operator-token", RoleOperator)
	if err != nil {
		t.Fatalf("add operator token: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/download", nil)
	req.Header.Set("X-Api-Key", operatorToken)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("backup download as operator: expected %d got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
}
