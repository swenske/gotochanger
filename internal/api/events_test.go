package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/swenske/gotochanger/internal/library"
)

func eventByCode(events []library.Event, code string) (library.Event, bool) {
	for _, e := range events {
		if e.Code == code {
			return e, true
		}
	}
	return library.Event{}, false
}

func TestSourceIPFromRequest_Priority(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	r.RemoteAddr = "192.0.2.10:12345"
	r.Header.Set("X-Real-IP", "198.51.100.7")
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.8")

	got := sourceIPFromRequest(r)
	if got != "203.0.113.9" {
		t.Fatalf("expected forwarded source IP, got %q", got)
	}
}

func TestEmitEventIncludesActorAndSourceIP(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/doors/io/open", nil)
	r.RemoteAddr = "203.0.113.41:54321"
	r = r.WithContext(withPrincipal(r.Context(), Principal{Subject: "alice", Role: RoleOperator, Via: "session"}))

	s.emitEvent(r, library.Event{Code: library.EventCodeRoboticsDoorIOOpenSuccess, Message: "opened IO mail slot door"})

	events := s.lib.Events()
	if len(events) == 0 {
		t.Fatalf("expected one event")
	}
	e := events[0]
	if e.Actor != "alice" {
		t.Fatalf("expected actor alice, got %q", e.Actor)
	}
	if e.Source != "session" {
		t.Fatalf("expected source session, got %q", e.Source)
	}
	if e.Detail["username"] != "alice" {
		t.Fatalf("expected username alice, got %q", e.Detail["username"])
	}
	if e.Detail["source_ip"] != "203.0.113.41" {
		t.Fatalf("expected source_ip 203.0.113.41, got %q", e.Detail["source_ip"])
	}
}

func TestPublicHandlerBackfillsLibraryEventsWithUsernameAndSourceIP(t *testing.T) {
	s := newPublicTestServer(t)
	h := s.PublicHandler()
	cookie := loginCookie(t, h, "Admin", "AdminPass123!")

	rr := httptest.NewRecorder()
	req := reqJSON(t, http.MethodPost, "/api/v1/tape-sets/"+testTapeSet+"/tapes", map[string]any{"barcode": "TAPE0001"})
	req.RemoteAddr = "198.51.100.77:10001"
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	e, ok := eventByCode(s.lib.Events(), library.EventCodeMediaOutsideCreateSuccess)
	if !ok {
		t.Fatalf("expected %s event", library.EventCodeMediaOutsideCreateSuccess)
	}
	if e.Actor != "Admin" {
		t.Fatalf("expected actor Admin, got %q", e.Actor)
	}
	if e.Detail["username"] != "Admin" {
		t.Fatalf("expected username Admin, got %q", e.Detail["username"])
	}
	if e.Detail["source_ip"] != "198.51.100.77" {
		t.Fatalf("expected source_ip 198.51.100.77, got %q", e.Detail["source_ip"])
	}
}
