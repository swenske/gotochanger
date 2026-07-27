package api

import (
	"net"
	"net/http"
	"strings"

	"github.com/swenske/gotochanger/internal/library"
)

func (s *Server) emitEvent(r *http.Request, evt library.Event) {
	if s == nil || s.lib == nil {
		return
	}
	s.lib.RecordEvent(s.enrichEvent(r, evt))
}

// enrichEvent fills in request/actor detail (source IP, method, path,
// actor/source/username) the same way emitEvent always has, without
// persisting anything. Split out so a caller that must not go through
// s.lib.RecordEvent - because RecordEvent's saveLocked() re-persists the
// *entire* current in-memory Library state, not just the event log, which
// would be actively wrong right after a database swap/reset - can still
// build a properly enriched event to persist by other means.
func (s *Server) enrichEvent(r *http.Request, evt library.Event) library.Event {
	if r != nil {
		if evt.Detail == nil {
			evt.Detail = map[string]string{}
		}
		if ip := sourceIPFromRequest(r); ip != "" {
			if _, ok := evt.Detail["source_ip"]; !ok {
				evt.Detail["source_ip"] = ip
			}
		}
		if _, ok := evt.Detail["method"]; !ok {
			evt.Detail["method"] = r.Method
		}
		if _, ok := evt.Detail["path"]; !ok {
			evt.Detail["path"] = r.URL.Path
		}
		if p, ok := principalFrom(r); ok {
			if evt.Actor == "" {
				evt.Actor = p.Subject
			}
			if evt.Source == "" {
				evt.Source = p.Via
			}
		}
		if evt.Actor != "" {
			if _, ok := evt.Detail["username"]; !ok {
				evt.Detail["username"] = evt.Actor
			}
		}
	}
	if evt.Source == "" {
		evt.Source = "anonymous"
	}
	return evt
}

func sourceIPFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		for _, p := range parts {
			if ip := parseIP(strings.TrimSpace(p)); ip != "" {
				return ip
			}
		}
	}
	if ip := parseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != "" {
		return ip
	}
	return parseIP(strings.TrimSpace(r.RemoteAddr))
}

func parseIP(raw string) string {
	if raw == "" {
		return ""
	}
	value := raw
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	if ip := net.ParseIP(value); ip != nil {
		return ip.String()
	}
	return ""
}

func (s *Server) emitFailure(r *http.Request, code, message string, err error, detail map[string]string) {
	if detail == nil {
		detail = map[string]string{}
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	s.emitEvent(r, library.Event{Code: code, Message: message, Detail: detail})
}
