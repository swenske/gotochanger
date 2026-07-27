package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/swenske/gotochanger/internal/library"
)

// sseMessage is one Server-Sent Event: event carries the SSE "event:" field,
// data the already-encoded "data:" payload.
type sseMessage struct {
	event string
	data  string
}

// Broadcaster fans out live updates to every subscribed SSE connection (see
// handleStream). It implements both library.Notifier and
// library.PhaseNotifier so a single instance can be wired as a subscriber of
// both the ordinary event stream and the door-phase live-update stream,
// without Library needing to know anything about SSE/HTTP.
//
// Ordinary events (Notify) carry the real library.Event as their payload.
// Subscribers still respond to an "update" message by calling the same
// refresh() the web UI already used for its old 4s poll loop (keeping
// every grid/card render function's data path unchanged), but also read
// the event straight off the message for the live robotic-activity panel.
// That second use is what the payload is actually for: Move/Load/Unload
// hold Library's main lock for their entire simulated sleep, and so does
// GET /api/v1/status's handler (Library.Status()) and GET /api/v1/events
// (Library.Events()) - a client that only reacted to a bare nudge by
// re-fetching one of those would simply block until the whole operation
// finished, so a burst of granular in-flight events would never actually
// be visible in real time. Notify itself is dispatched via "go
// l.notifier.Notify(e)" from inside Library.emit, which touches neither
// Library's lock nor its internals, so delivering the payload here is
// already lock-free - no second phase-map-style mechanism is needed the
// way it was for doors. Door-phase transitions (NotifyPhase) remain a
// genuinely separate case: they carry a kind/id/phase payload because they
// have no corresponding Event at all (a phase is a transient live signal,
// deliberately never part of the audited event log - see PhaseNotifier's
// doc comment).
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan sseMessage]struct{}
}

// NewBroadcaster returns a ready-to-use Broadcaster.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[chan sseMessage]struct{}{}}
}

// Subscribe registers a new subscriber and returns its message channel,
// buffered a handful deep so a short burst of transitions (at most a
// few per single door operation) doesn't get dropped by a slow reader,
// without ever blocking the caller (see broadcast). The caller must call
// Unsubscribe when done (e.g. via defer) to avoid leaking the entry.
func (b *Broadcaster) Subscribe() chan sseMessage {
	ch := make(chan sseMessage, 8)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes ch, registered by a prior Subscribe call.
func (b *Broadcaster) Unsubscribe(ch chan sseMessage) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// broadcast delivers msg to every subscriber without ever blocking - a full
// subscriber channel (an unresponsive/slow tab) just drops the message
// rather than stalling the caller. This matters more than usual for phase
// messages, since NotifyPhase is called synchronously from inside a door
// method that already holds Library's main lock: a blocking send here
// could hang the entire library, not just one subscriber.
func (b *Broadcaster) broadcast(msg sseMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Notify implements library.Notifier.
func (b *Broadcaster) Notify(e library.Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		payload = []byte("{}")
	}
	b.broadcast(sseMessage{event: "update", data: string(payload)})
}

// NotifyPhase implements library.PhaseNotifier.
func (b *Broadcaster) NotifyPhase(kind, id, phase string) {
	b.broadcast(sseMessage{event: "phase", data: encodePhaseMessage(kind, id, phase)})
}

func encodePhaseMessage(kind, id, phase string) string {
	payload, _ := json.Marshal(struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Phase string `json:"phase"`
	}{Kind: kind, ID: id, Phase: phase})
	return string(payload)
}

// NotifyArm implements library.PhaseNotifier.
func (b *Broadcaster) NotifyArm(state library.ArmState, step library.ArmStep) {
	b.broadcast(sseMessage{event: "arm", data: encodeArmMessage(state, step)})
}

func encodeArmMessage(state library.ArmState, step library.ArmStep) string {
	payload, _ := json.Marshal(struct {
		Busy     bool                `json:"busy"`
		Position library.ArmPosition `json:"position"`
		Step     string              `json:"step,omitempty"`
		StepTime time.Time           `json:"step_time,omitempty"`
	}{Busy: state.Busy, Position: state.Position, Step: step.Message, StepTime: step.Time})
	return string(payload)
}

// writeSSE writes one complete SSE message (event + data + terminating
// blank line) and flushes it immediately.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	flusher.Flush()
}

// handleStream serves GET /api/v1/stream: a Server-Sent Events connection
// that pushes a message every time something in the library changes,
// letting the dashboard replace its old fixed-interval poll with genuine
// live updates while falling back to that same poll if the connection
// drops.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if s.broadcaster == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("live updates are not available"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch := s.broadcaster.Subscribe()
	defer s.broadcaster.Unsubscribe(ch)

	// Nudge immediately so a freshly opened tab doesn't wait for the next
	// change before its first refresh.
	writeSSE(w, flusher, "update", "{}")

	// Catch a freshly (re)connected client up on any door phase already in
	// flight - DoorPhases() never blocks on Library's main lock (see
	// Broadcaster's doc comment), so this is safe to call even while
	// another request is stuck waiting on a door method's sleep.
	if s.lib != nil {
		for key, phase := range s.lib.DoorPhases() {
			kind, id, ok := strings.Cut(key, ":")
			if !ok {
				continue
			}
			writeSSE(w, flusher, "phase", encodePhaseMessage(kind, id, phase))
		}

		// Same catch-up idea for the robotic arm: one message with the
		// current busy/position state (no step), then one per buffered
		// live-only narration step (see ArmSteps' doc comment) - each
		// still carrying the *current* state alongside that historical
		// step, the same harmless-repeat pattern the phase replay above
		// already uses. ArmState/ArmSteps never block on Library's main
		// lock either.
		writeSSE(w, flusher, "arm", encodeArmMessage(s.lib.ArmState(), library.ArmStep{}))
		for _, step := range s.lib.ArmSteps() {
			writeSSE(w, flusher, "arm", encodeArmMessage(s.lib.ArmState(), step))
		}
	}

	const keepaliveInterval = 20 * time.Second
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			writeSSE(w, flusher, msg.event, msg.data)
		case <-keepalive.C:
			// SSE comment line: keeps idle proxies/load balancers from
			// timing out a connection with no real traffic on it.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
