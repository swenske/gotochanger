package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendPostsExpectedPayloadAndHeaders(t *testing.T) {
	var gotBody Payload
	var gotContentType, gotUserAgent, gotMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		gotUserAgent = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	payload := Payload{
		InstanceID:  "abc123",
		Version:     "1.2.3",
		OS:          "linux",
		Arch:        "amd64",
		DrivesTotal: 4,
		SlotsTotal:  40,
	}

	if err := Send(context.Background(), srv.Client(), srv.URL, payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotUserAgent != "gotochanger/1.2.3" {
		t.Fatalf("User-Agent = %q, want gotochanger/1.2.3", gotUserAgent)
	}
	if gotBody != payload {
		t.Fatalf("received payload = %+v, want %+v", gotBody, payload)
	}
}

func TestSendNonSuccessStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := Send(context.Background(), srv.Client(), srv.URL, Payload{}); err == nil {
		t.Fatal("Send: want error for a 500 response, got nil")
	}
}

func TestSendExpiredContextIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)

	if err := Send(ctx, srv.Client(), srv.URL, Payload{}); err == nil {
		t.Fatal("Send: want error for an already-expired context, got nil")
	}
}

func TestSendUnreachableEndpointIsAnError(t *testing.T) {
	if err := Send(context.Background(), http.DefaultClient, "http://127.0.0.1:1/no-such-server", Payload{}); err == nil {
		t.Fatal("Send: want error for an unreachable endpoint, got nil")
	}
}
