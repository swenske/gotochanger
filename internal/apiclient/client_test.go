package apiclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoReturnsAPIErrorWithStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"robotic arm: blocked_arm: robotic arm is in fault state"}`))
	}))
	defer srv.Close()

	c := NewHTTP(srv.URL, "")
	_, err := c.Status()
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusConflict)
	}
	if want := "GET /api/v1/status: robotic arm: blocked_arm: robotic arm is in fault state"; apiErr.Message != want {
		t.Errorf("Message = %q, want %q", apiErr.Message, want)
	}
	if err.Error() != apiErr.Message {
		t.Errorf("err.Error() = %q, want %q (unchanged from before APIError existed)", err.Error(), apiErr.Message)
	}
}

func TestPostBytesReturnsAPIErrorWithStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"uploaded file is not a SQLite database"}`))
	}))
	defer srv.Close()

	c := NewHTTP(srv.URL, "")
	err := c.Restore([]byte("not a database"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusBadRequest)
	}
}
