// Package telemetry defines the anonymous usage-statistics payload gotochangerd
// may optionally send, and the primitive that sends it.
//
// It has no dependency on any other internal package (same "leaf package"
// convention as internal/secrethash, internal/instanceid) - callers in
// internal/api build the Payload from live library/store state and decide
// when/whether to call Send.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// DefaultEndpoint is where gotochangerd reports anonymous usage statistics
// when an operator has opted in (see internal/api/telemetry.go). The
// collector doesn't exist yet as of this package's introduction - Send
// simply returns an error until one does, which is fine given Send is
// always called best-effort and its result is never surfaced to any
// request path (see sendTelemetryAsync's doc comment).
const DefaultEndpoint = "https://gotochanger.sw-servers.net/v1/ping"

// Payload is the entire anonymous usage-statistics report - deliberately
// flat and small. It never includes anything that identifies an
// installation's owner or contents: no VTL name, no magazine/mailbox/
// drive/tape-type names, no volume barcodes or paths, no usernames, no
// IP addresses or hostnames. InstanceID is itself already an anonymized,
// one-way-hashed (or, failing that, purely random) value - see
// internal/instanceid.
type Payload struct {
	InstanceID  string `json:"instance_id"`
	Version     string `json:"version"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	InContainer bool   `json:"in_container"`

	OperationalMode  string `json:"operational_mode"`
	KernelModeActive bool   `json:"kernel_mode_active"`

	DrivesTotal           int `json:"drives_total"`
	SlotsTotal            int `json:"slots_total"`
	IOSlotsTotal          int `json:"ioslots_total"`
	MagazinesTotal        int `json:"magazines_total"`
	MailboxesTotal        int `json:"mailboxes_total"`
	LogicalLibrariesTotal int `json:"logical_libraries_total"`
	TapeSetsTotal         int `json:"tape_sets_total"`
	VolumesTotal          int `json:"volumes_total"`
	UsersTotal            int `json:"users_total"`

	SNMPEnabled            bool `json:"snmp_enabled"`
	PrometheusEnabled      bool `json:"prometheus_enabled"`
	OffsiteRotationEnabled bool `json:"offsite_rotation_enabled"`
	CleaningSimEnabled     bool `json:"cleaning_enabled"`
	LatencySimEnabled      bool `json:"latency_simulation_enabled"`
}

// Send POSTs payload as JSON to endpoint using client, respecting
// whatever deadline ctx carries. Best-effort: the caller decides how a
// non-nil error is handled (internal/api/telemetry.go logs it at Debug
// and otherwise ignores it - a failed send must never affect any request
// path or startup).
func Send(ctx context.Context, client *http.Client, endpoint string, payload Payload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode telemetry payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build telemetry request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gotochanger/"+payload.Version)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send telemetry: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry endpoint returned %s", resp.Status)
	}
	return nil
}
