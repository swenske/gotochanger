// Package apiclient is a small HTTP client used by the CLI tools
// (gotochangerctl, gotochanger-changer) to talk to the gotochangerd REST
// API, either over the trusted local Unix socket or over TCP with a token.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
)

// Client talks to the gotochangerd API.
type Client struct {
	http           *http.Client
	base           string
	token          string
	logicalLibrary string
}

// DefaultTimeout bounds how long a client waits for one request.
//
// This is deliberately generous, not the 15s it used to be: gotochangerd
// simulates real tape-library timing by sleeping while holding its single
// library lock, so a perfectly healthy call can legitimately take minutes.
// Closing a magazine door at the factory-default latency settings already
// costs 20s (door_action 2s + robot_move_scan 6s + magazine_scan 12s), and
// each of the seven delays can be tuned up to 5m - on top of which the
// single-robotic-arm model queues every other operation behind whichever
// one holds the lock, so no per-call value can be derived from the delays
// alone anyway. The old timeout made `gotochangerctl storage-door close`
// fail against a working daemon at stock settings.
//
// It still exists (rather than being disabled outright) so a genuinely
// wedged daemon eventually returns an error instead of hanging forever;
// callers that want to fail faster can use SetTimeout.
const DefaultTimeout = 10 * time.Minute

// NewUnix builds a Client that dials the trusted local Unix domain socket.
// No token is required or sent: access is controlled by the socket's
// filesystem permissions.
func NewUnix(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: DefaultTimeout}, base: "http://unix", logicalLibrary: ""}
}

// NewHTTP builds a Client that talks to the TCP listener using an API
// token.
func NewHTTP(baseURL, token string) *Client {
	return &Client{http: &http.Client{Timeout: DefaultTimeout}, base: baseURL, token: token, logicalLibrary: ""}
}

// SetTimeout overrides DefaultTimeout for this client.
func (c *Client) SetTimeout(d time.Duration) { c.http.Timeout = d }

// APIError is returned by do/getBytes/postBytes for any response with a
// 4xx/5xx status - it carries the HTTP status code alongside the same
// message text those methods have always produced, so an existing caller
// that only checks "err != nil" sees no change at all, while a caller
// that needs to distinguish causes (e.g. internal/scsi mapping a Library
// error to a specific SCSI sense code for kernel mode) can recover the
// status via errors.As.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

func (c *Client) do(method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-Api-Key", c.token)
	}
	if c.logicalLibrary != "" {
		req.Header.Set("X-Logical-Library", c.logicalLibrary)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &APIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("%s %s: %s", method, path, e.Error)}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) Status() (library.Status, error) {
	var st library.Status
	err := c.do(http.MethodGet, "/api/v1/status", nil, &st)
	return st, err
}

func (c *Client) Events() ([]library.Event, error) {
	var evs []library.Event
	err := c.do(http.MethodGet, "/api/v1/events", nil, &evs)
	return evs, err
}

func (c *Client) Volumes() ([]*library.Volume, error) {
	var vs []*library.Volume
	err := c.do(http.MethodGet, "/api/v1/volumes", nil, &vs)
	return vs, err
}

func (c *Client) OutsideVolumes() ([]*library.Volume, error) {
	var vs []*library.Volume
	err := c.do(http.MethodGet, "/api/v1/outside", nil, &vs)
	return vs, err
}

func (c *Client) Load(fromKind string, fromAddr, drive int) error {
	return c.do(http.MethodPost, "/api/v1/load", map[string]any{
		"from_kind": fromKind, "from_address": fromAddr, "drive": drive,
	}, nil)
}

func (c *Client) Unload(drive int, toKind string, toAddr int) error {
	return c.do(http.MethodPost, "/api/v1/unload", map[string]any{
		"drive": drive, "to_kind": toKind, "to_address": toAddr,
	}, nil)
}

func (c *Client) Move(fromKind string, fromAddr int, toKind string, toAddr int) error {
	return c.do(http.MethodPost, "/api/v1/move", map[string]any{
		"from_kind": fromKind, "from_address": fromAddr, "to_kind": toKind, "to_address": toAddr,
	}, nil)
}

func (c *Client) DeleteOutsideVolume(barcode string) error {
	return c.do(http.MethodDelete, "/api/v1/outside/"+barcode, nil, nil)
}

func (c *Client) OpenIODoor(mailboxID, pin string) error {
	return c.do(http.MethodPost, fmt.Sprintf("/api/v1/doors/io/%s/open", mailboxID), map[string]any{"pin": pin}, nil)
}

func (c *Client) CloseIODoor(mailboxID string, actions []library.DoorAction) error {
	return c.do(http.MethodPost, fmt.Sprintf("/api/v1/doors/io/%s/close", mailboxID), map[string]any{"actions": actions}, nil)
}

func (c *Client) OpenStorageDoor(magazineID, pin string) error {
	return c.do(http.MethodPost, fmt.Sprintf("/api/v1/doors/storage/%s/open", magazineID), map[string]any{"pin": pin}, nil)
}

func (c *Client) CloseStorageDoor(magazineID string, actions []library.DoorAction) error {
	return c.do(http.MethodPost, fmt.Sprintf("/api/v1/doors/storage/%s/close", magazineID), map[string]any{"actions": actions}, nil)
}

func (c *Client) DriveFault(index int, fault bool) error {
	return c.do(http.MethodPost, fmt.Sprintf("/api/v1/drives/%d/fault", index), map[string]any{"fault": fault}, nil)
}

func (c *Client) SetVolumeWriteProtect(barcode string, protected bool) error {
	return c.do(http.MethodPost, fmt.Sprintf("/api/v1/volumes/%s/write-protect", barcode), map[string]any{"write_protected": protected}, nil)
}

func (c *Client) RoboticFault(active bool, kind, message string) error {
	return c.do(http.MethodPost, "/api/v1/robotics/fault", map[string]any{"active": active, "kind": kind, "message": message}, nil)
}

// SetLogicalLibrary sets the logical library to use for subsequent requests.
func (c *Client) SetLogicalLibrary(name string) {
	c.logicalLibrary = name
}

func (c *Client) CreateToken(name, role string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(http.MethodPost, "/api/v1/tokens", map[string]any{"name": name, "role": role}, &out)
	return out.Token, err
}

func (c *Client) RevokeToken(name string) error {
	return c.do(http.MethodDelete, "/api/v1/tokens/"+name, nil, nil)
}

func (c *Client) ListTokens() ([]TokenInfo, error) {
	var out []TokenInfo
	err := c.do(http.MethodGet, "/api/v1/tokens", nil, &out)
	return out, err
}

// TokenInfo mirrors api.TokenRecord without exposing the token hash type
// from the server package.
type TokenInfo struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
}

// UserInfo mirrors api.UserInfo.
type UserInfo struct {
	Username           string    `json:"username"`
	Role               string    `json:"role"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (c *Client) ListUsers() ([]UserInfo, error) {
	var out []UserInfo
	err := c.do(http.MethodGet, "/api/v1/users", nil, &out)
	return out, err
}

func (c *Client) CreateUser(username, role, password string) (UserInfo, error) {
	var out UserInfo
	err := c.do(http.MethodPost, "/api/v1/users", map[string]any{"username": username, "role": role, "password": password}, &out)
	return out, err
}

func (c *Client) DeleteUser(username string) error {
	return c.do(http.MethodDelete, "/api/v1/users/"+username, nil, nil)
}

func (c *Client) SetUserRole(username, role string) error {
	return c.do(http.MethodPost, "/api/v1/users/"+username+"/role", map[string]any{"role": role}, nil)
}

func (c *Client) ResetUserPassword(username, newPassword string) error {
	return c.do(http.MethodPost, "/api/v1/users/"+username+"/reset-password", map[string]any{"new_password": newPassword}, nil)
}

// GetSettings returns the raw JSON settings document (config + which fields
// require a restart), left as json.RawMessage so the CLI can pretty-print
// or forward it without needing to mirror every config field.
func (c *Client) GetSettings() (map[string]any, error) {
	var out map[string]any
	err := c.do(http.MethodGet, "/api/v1/settings", nil, &out)
	return out, err
}

// UpdateSettings sends a partial settings update (only non-nil/non-zero
// fields set by the caller in req should be included).
func (c *Client) UpdateSettings(req map[string]any) (map[string]any, error) {
	var out map[string]any
	err := c.do(http.MethodPut, "/api/v1/settings", req, &out)
	return out, err
}

// LatencySettingsInfo mirrors api.LatencySettingsResult without importing
// the api package (see WizardStateInfo's doc comment for why).
type LatencySettingsInfo struct {
	Settings config.LatencySettings `json:"settings"`
	Defaults config.LatencySettings `json:"defaults"`
}

// GetLatencySettings returns the effective and factory-default latency
// simulation delays.
func (c *Client) GetLatencySettings() (LatencySettingsInfo, error) {
	var out LatencySettingsInfo
	err := c.do(http.MethodGet, "/api/v1/settings/latency", nil, &out)
	return out, err
}

// UpdateLatencySettings replaces the full set of latency simulation
// delays (unlike UpdateSettings, this is not a partial update - every
// field in ls is authoritative, matching PUT /api/v1/settings/latency's
// contract).
func (c *Client) UpdateLatencySettings(ls config.LatencySettings) (LatencySettingsInfo, error) {
	var out LatencySettingsInfo
	err := c.do(http.MethodPut, "/api/v1/settings/latency", ls, &out)
	return out, err
}

// PrometheusSettingsInfo mirrors api.prometheusSettingsResponse without
// importing the api package (see WizardStateInfo's doc comment for why).
type PrometheusSettingsInfo struct {
	Enabled     bool   `json:"enabled"`
	MetricsPath string `json:"metrics_path"`
}

// GetPrometheusSettings returns whether the Prometheus /metrics exporter is
// enabled, plus its (fixed) scrape path.
func (c *Client) GetPrometheusSettings() (PrometheusSettingsInfo, error) {
	var out PrometheusSettingsInfo
	err := c.do(http.MethodGet, "/api/v1/settings/prometheus", nil, &out)
	return out, err
}

// UpdatePrometheusSettings enables or disables the Prometheus /metrics
// exporter.
func (c *Client) UpdatePrometheusSettings(enabled bool) (PrometheusSettingsInfo, error) {
	var out PrometheusSettingsInfo
	err := c.do(http.MethodPut, "/api/v1/settings/prometheus", map[string]bool{"enabled": enabled}, &out)
	return out, err
}

// DownloadGrafanaDashboard returns the pre-built Grafana dashboard JSON for
// this daemon's Prometheus metrics.
func (c *Client) DownloadGrafanaDashboard() ([]byte, error) {
	data, _, err := c.getBytes("/api/v1/prometheus/dashboard")
	return data, err
}

// CleaningSettingsInfo mirrors api.CleaningSettingsResult without
// importing the api package (see WizardStateInfo's doc comment for why).
type CleaningSettingsInfo struct {
	Settings config.CleaningSettings `json:"settings"`
	Defaults config.CleaningSettings `json:"defaults"`
}

// GetCleaningSettings returns the effective and factory-default
// cleaning-tape management settings.
func (c *Client) GetCleaningSettings() (CleaningSettingsInfo, error) {
	var out CleaningSettingsInfo
	err := c.do(http.MethodGet, "/api/v1/settings/cleaning", nil, &out)
	return out, err
}

// UpdateCleaningSettings replaces the full set of cleaning-tape
// management settings (not a partial update, matching
// UpdateLatencySettings' own contract).
func (c *Client) UpdateCleaningSettings(cs config.CleaningSettings) (CleaningSettingsInfo, error) {
	var out CleaningSettingsInfo
	err := c.do(http.MethodPut, "/api/v1/settings/cleaning", cs, &out)
	return out, err
}

// ---- cleaning tapes ----

// ListCleaningTapes returns every cleaning cartridge in the pool,
// regardless of where it currently sits.
func (c *Client) ListCleaningTapes() ([]*library.Volume, error) {
	var vs []*library.Volume
	err := c.do(http.MethodGet, "/api/v1/cleaning/tapes", nil, &vs)
	return vs, err
}

// CreateCleaningTape generates and creates one new cleaning cartridge
// (up to the pool's fixed maximum). Manual cleaning has no separate
// trigger - an operator runs a cleaning cycle simply by Load()ing a
// cleaning cartridge into a drive, same as loading any other volume.
func (c *Client) CreateCleaningTape() (*library.Volume, error) {
	var v library.Volume
	err := c.do(http.MethodPost, "/api/v1/cleaning/tapes", map[string]any{}, &v)
	return &v, err
}

// ---- logical libraries ----

func (c *Client) ListLogicalLibraries() ([]*library.LogicalLibrary, error) {
	var out []*library.LogicalLibrary
	err := c.do(http.MethodGet, "/api/v1/logical-libraries", nil, &out)
	return out, err
}

func (c *Client) GetLogicalLibrary(name string) (*library.LogicalLibrary, error) {
	var out library.LogicalLibrary
	err := c.do(http.MethodGet, "/api/v1/logical-libraries/"+name, nil, &out)
	return &out, err
}

func (c *Client) CreateLogicalLibrary(lib config.LogicalLibraryConfig) (*library.LogicalLibrary, error) {
	var out library.LogicalLibrary
	err := c.do(http.MethodPost, "/api/v1/logical-libraries", lib, &out)
	return &out, err
}

func (c *Client) UpdateLogicalLibrary(name string, lib config.LogicalLibraryConfig) (*library.LogicalLibrary, error) {
	var out library.LogicalLibrary
	err := c.do(http.MethodPut, "/api/v1/logical-libraries/"+name, lib, &out)
	return &out, err
}

func (c *Client) DeleteLogicalLibrary(name string) error {
	return c.do(http.MethodDelete, "/api/v1/logical-libraries/"+name, nil, nil)
}

// UnassignedElements is the shape returned by GET /api/v1/unassigned.
type UnassignedElements struct {
	Drives  []*library.Drive  `json:"drives"`
	Slots   []*library.Slot   `json:"slots"`
	IOSlots []*library.IOSlot `json:"ioslots"`
}

func (c *Client) UnassignedElements() (UnassignedElements, error) {
	var out UnassignedElements
	err := c.do(http.MethodGet, "/api/v1/unassigned", nil, &out)
	return out, err
}

// ---- drive types ----

func (c *Client) ListDriveTypes() ([]config.DriveType, error) {
	var out []config.DriveType
	err := c.do(http.MethodGet, "/api/v1/drive-types", nil, &out)
	return out, err
}

func (c *Client) CreateDriveType(dt config.DriveType) error {
	return c.do(http.MethodPost, "/api/v1/drive-types", dt, nil)
}

func (c *Client) UpdateDriveType(name string, dt config.DriveType) error {
	return c.do(http.MethodPut, "/api/v1/drive-types/"+name, dt, nil)
}

func (c *Client) DeleteDriveType(name string) error {
	return c.do(http.MethodDelete, "/api/v1/drive-types/"+name, nil, nil)
}

// ---- tape types ----

func (c *Client) ListTapeTypes() ([]config.TapeType, error) {
	var out []config.TapeType
	err := c.do(http.MethodGet, "/api/v1/tape-types", nil, &out)
	return out, err
}

func (c *Client) CreateTapeType(tt config.TapeType) error {
	return c.do(http.MethodPost, "/api/v1/tape-types", tt, nil)
}

func (c *Client) UpdateTapeType(name string, tt config.TapeType) error {
	return c.do(http.MethodPut, "/api/v1/tape-types/"+name, tt, nil)
}

func (c *Client) DeleteTapeType(name string) error {
	return c.do(http.MethodDelete, "/api/v1/tape-types/"+name, nil, nil)
}

// ---- tape sets ----

func (c *Client) ListTapeSets() ([]config.TapeSetConfig, error) {
	var out []config.TapeSetConfig
	err := c.do(http.MethodGet, "/api/v1/tape-sets", nil, &out)
	return out, err
}

// CreateTapeSetResult is the response shape for POST /api/v1/tape-sets: the
// created tape set plus the cartridges generated for its initial tape count.
type CreateTapeSetResult struct {
	config.TapeSetConfig
	Cartridges []*library.Volume `json:"cartridges"`
}

// CreateTapeSet creates a new tape set and immediately generates
// initialTapeCount cartridges for it, barcoded per its tape type's format.
func (c *Client) CreateTapeSet(name, tapeType, storageFolder string, initialTapeCount int) (CreateTapeSetResult, error) {
	var out CreateTapeSetResult
	err := c.do(http.MethodPost, "/api/v1/tape-sets", map[string]any{
		"name": name, "tape_type": tapeType, "storage_folder": storageFolder, "initial_tape_count": initialTapeCount,
	}, &out)
	return out, err
}

func (c *Client) UpdateTapeSet(name string, ts config.TapeSetConfig) error {
	return c.do(http.MethodPut, "/api/v1/tape-sets/"+name, ts, nil)
}

func (c *Client) DeleteTapeSet(name string) error {
	return c.do(http.MethodDelete, "/api/v1/tape-sets/"+name, nil, nil)
}

// AddTapeSetTapes bulk-generates count new cartridges for an existing tape
// set, auto-barcoded per its tape type's format. Safe to call repeatedly to
// top up a tape set.
func (c *Client) AddTapeSetTapes(name string, count int) ([]*library.Volume, error) {
	var out []*library.Volume
	err := c.do(http.MethodPost, fmt.Sprintf("/api/v1/tape-sets/%s/tapes", name), map[string]any{"count": count}, &out)
	return out, err
}

// AddTapeSetTape creates exactly one cartridge in an existing tape set with
// an operator-supplied barcode.
func (c *Client) AddTapeSetTape(name, barcode string) (*library.Volume, error) {
	var out []*library.Volume
	err := c.do(http.MethodPost, fmt.Sprintf("/api/v1/tape-sets/%s/tapes", name), map[string]any{"barcode": barcode}, &out)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

// GetFSBrowse mirrors GET /api/v1/fs/browse, backing the Admin UI's
// tape-set storage-folder picker.
type FSBrowseResult struct {
	Path    string `json:"path"`
	Parent  string `json:"parent,omitempty"`
	Entries []struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
	} `json:"entries"`
}

func (c *Client) FSBrowse(path string) (FSBrowseResult, error) {
	var out FSBrowseResult
	q := ""
	if path != "" {
		q = "?path=" + url.QueryEscape(path)
	}
	err := c.do(http.MethodGet, "/api/v1/fs/browse"+q, nil, &out)
	return out, err
}

// ---- magazines ----

// MagazineInfo mirrors the Admin API's magazine response DTO
// (internal/api/handlers_topology.go's magazineView) - not just
// config.MagazineConfig, since the API additionally reports Ordinal, this
// magazine's live 1-based position (matching the number before the dot in
// its slots' Label), recomputed on every response and never stored.
type MagazineInfo struct {
	ID      string `json:"id"`
	Slots   int    `json:"slots"`
	Ordinal int    `json:"ordinal"`
}

func (c *Client) ListMagazines() ([]MagazineInfo, error) {
	var out []MagazineInfo
	err := c.do(http.MethodGet, "/api/v1/magazines", nil, &out)
	return out, err
}

func (c *Client) CreateMagazine(m config.MagazineConfig) error {
	return c.do(http.MethodPost, "/api/v1/magazines", m, nil)
}

func (c *Client) UpdateMagazine(id string, m config.MagazineConfig) error {
	return c.do(http.MethodPut, "/api/v1/magazines/"+id, m, nil)
}

func (c *Client) DeleteMagazine(id string) error {
	return c.do(http.MethodDelete, "/api/v1/magazines/"+id, nil, nil)
}

// ---- mailboxes ----

// MailboxInfo mirrors the Admin API's mailbox response DTO
// (internal/api/handlers_topology.go's mailboxView) - see MagazineInfo's
// doc comment; Ordinal is numbered independently from magazines' (see
// library.IOSlot.Label).
type MailboxInfo struct {
	ID      string `json:"id"`
	Slots   int    `json:"slots"`
	Ordinal int    `json:"ordinal"`
	PINSet  bool   `json:"pin_set"`
}

func (c *Client) ListMailboxes() ([]MailboxInfo, error) {
	var out []MailboxInfo
	err := c.do(http.MethodGet, "/api/v1/mailboxes", nil, &out)
	return out, err
}

func (c *Client) CreateMailbox(m config.MailboxConfig) error {
	return c.do(http.MethodPost, "/api/v1/mailboxes", m, nil)
}

func (c *Client) UpdateMailbox(id string, m config.MailboxConfig) error {
	return c.do(http.MethodPut, "/api/v1/mailboxes/"+id, m, nil)
}

func (c *Client) DeleteMailbox(id string) error {
	return c.do(http.MethodDelete, "/api/v1/mailboxes/"+id, nil, nil)
}

// ---- drive devices ----

func (c *Client) ListDrives() ([]library.Drive, error) {
	var out []library.Drive
	err := c.do(http.MethodGet, "/api/v1/drives", nil, &out)
	return out, err
}

// CreateDrive appends a new drive device. An empty devicePath asks the
// server to auto-generate one under its data directory.
func (c *Client) CreateDrive(devicePath string) (library.Drive, error) {
	var out library.Drive
	err := c.do(http.MethodPost, "/api/v1/drives", map[string]string{"device_path": devicePath}, &out)
	return out, err
}

func (c *Client) UpdateDrive(index int, devicePath string) error {
	return c.do(http.MethodPut, fmt.Sprintf("/api/v1/drives/%d", index), map[string]string{"device_path": devicePath}, nil)
}

func (c *Client) DeleteDrive(index int) error {
	return c.do(http.MethodDelete, fmt.Sprintf("/api/v1/drives/%d", index), nil, nil)
}

// ---- offsite vault ----

func (c *Client) OffsiteVolumes() ([]*library.Volume, error) {
	var vs []*library.Volume
	err := c.do(http.MethodGet, "/api/v1/offsite", nil, &vs)
	return vs, err
}

func (c *Client) OffsiteSend(fromKind string, fromAddr int) (*library.Volume, error) {
	var v library.Volume
	err := c.do(http.MethodPost, "/api/v1/offsite/send", map[string]any{
		"from_kind": fromKind, "from_address": fromAddr,
	}, &v)
	return &v, err
}

func (c *Client) OffsiteRecall(barcode, toKind string, toAddr int) error {
	return c.do(http.MethodPost, "/api/v1/offsite/recall", map[string]any{
		"barcode": barcode, "to_kind": toKind, "to_address": toAddr,
	}, nil)
}

// ---- wizard ----

// KernelModeStatusInfo mirrors api.KernelModeStatus without importing the
// api package (see WizardStateInfo's doc comment for why).
type KernelModeStatusInfo struct {
	Available           bool `json:"available"`
	MissingPackage      bool `json:"missing_package"`
	MissingKernelModule bool `json:"missing_kernel_module"`
}

// KernelModeStatus reports whether this host can run gotochanger-tcmud -
// see GET /api/v1/kernel-mode/status.
func (c *Client) KernelModeStatus() (KernelModeStatusInfo, error) {
	var out KernelModeStatusInfo
	err := c.do(http.MethodGet, "/api/v1/kernel-mode/status", nil, &out)
	return out, err
}

// KernelModeDrivePathsInfo mirrors api.KernelModeDrivePaths without
// importing the api package (see WizardStateInfo's doc comment for why).
type KernelModeDrivePathsInfo struct {
	Generic string `json:"generic"`
	Tape    string `json:"tape,omitempty"`

	// StableGeneric/StableTape are the /dev/tape/by-id/... paths udev
	// derives from this device's VPD page 0x83 identity (see
	// internal/scsi/vpd.go, internal/tcmu.DiscoverStablePaths) - stable
	// across a gotochanger-tcmud restart even though Generic/Tape above
	// (the raw kernel-assigned /dev/sgN/dev/nstN numbers) are not. Empty
	// until udev has processed the device.
	StableGeneric string `json:"stable_generic,omitempty"`
	StableTape    string `json:"stable_tape,omitempty"`
}

// KernelModeDeviceReportInfo mirrors api.KernelModeDeviceReport - what one
// gotochanger-tcmud instance reports about the real device paths the
// kernel assigned it (see ReportKernelModeDevices).
type KernelModeDeviceReportInfo struct {
	Changer string `json:"changer"`
	// ChangerStable is the changer's own stable by-id path, kept as a
	// separate additive field rather than widening Changer to the full
	// KernelModeDrivePathsInfo shape - that would carry a permanently
	// meaningless Tape/StableTape pair for a changer, and force every
	// existing "Changer: \"/dev/sg4\"" test literal to be rewritten for
	// no real benefit.
	ChangerStable string                           `json:"changer_stable,omitempty"`
	Drives        map[int]KernelModeDrivePathsInfo `json:"drives"`
}

// ReportKernelModeDevices tells gotochangerd what real device paths the
// kernel assigned this gotochanger-tcmud instance, for the Admin UI/
// Bareos Config generator to display - instance is the same name used in
// the systemd unit ("default" for the whole-physical-library case, or a
// logical library's own name).
func (c *Client) ReportKernelModeDevices(instance string, report KernelModeDeviceReportInfo) error {
	return c.do(http.MethodPost, "/api/v1/kernel-mode/devices/"+instance, report, nil)
}

// ClearKernelModeDevices removes a previously-reported device set - called
// on a clean shutdown so stale device paths don't linger in the Admin UI.
func (c *Client) ClearKernelModeDevices(instance string) error {
	return c.do(http.MethodDelete, "/api/v1/kernel-mode/devices/"+instance, nil, nil)
}

// ListKernelModeDevices returns every currently-reported device set, keyed
// by instance name.
func (c *Client) ListKernelModeDevices() (map[string]KernelModeDeviceReportInfo, error) {
	var out map[string]KernelModeDeviceReportInfo
	err := c.do(http.MethodGet, "/api/v1/kernel-mode/devices", nil, &out)
	return out, err
}

// WizardStateInfo mirrors api.WizardResponse without importing the api
// package (avoiding an import cycle - api already imports apiclient's
// sibling packages transitively through config/library).
type WizardStateInfo struct {
	Completed        bool                          `json:"completed"`
	CurrentStep      int                           `json:"current_step"`
	VTLName          string                        `json:"vtl_name,omitempty"`
	OperationalMode  string                        `json:"operational_mode,omitempty"`
	Drives           []config.DriveType            `json:"drives,omitempty"`
	Magazines        []config.MagazineConfig       `json:"magazines,omitempty"`
	Mailboxes        []config.MailboxConfig        `json:"mailboxes,omitempty"`
	OffsiteLocation  bool                          `json:"offsite_location,omitempty"`
	TapeSets         []config.TapeSetConfig        `json:"tape_sets,omitempty"`
	LogicalLibraries []config.LogicalLibraryConfig `json:"logical_libraries,omitempty"`
	LatencyEnabled   bool                          `json:"latency_enabled,omitempty"`
	DriveTypes       []config.DriveType            `json:"drive_types,omitempty"`
	TapeTypes        []config.TapeType             `json:"tape_types,omitempty"`
	KernelMode       KernelModeStatusInfo          `json:"kernel_mode"`
}

func (c *Client) WizardState() (WizardStateInfo, error) {
	var out WizardStateInfo
	err := c.do(http.MethodGet, "/api/v1/wizard", nil, &out)
	return out, err
}

func (c *Client) WizardOptions() (WizardStateInfo, error) {
	var out WizardStateInfo
	err := c.do(http.MethodGet, "/api/v1/wizard/options", nil, &out)
	return out, err
}

// ---- backup / restore ----

// BackupFileInfo mirrors store.BackupFileInfo without importing internal/store.
type BackupFileInfo struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// BackupScheduleInfo mirrors api.BackupScheduleInfo without importing internal/api.
type BackupScheduleInfo struct {
	Interval  string `json:"interval"`
	Retention int    `json:"retention"`
	LastRun   string `json:"last_run,omitempty"`
}

// getBytes performs a raw GET, returning the response body and the filename
// suggested by its Content-Disposition header (if any) - used for the
// backup download endpoints, whose responses aren't JSON.
func (c *Client) getBytes(path string) ([]byte, string, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, "", err
	}
	if c.token != "" {
		req.Header.Set("X-Api-Key", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, "", &APIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("GET %s: %s", path, e.Error)}
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}
	filename := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		filename = params["filename"]
	}
	return data, filename, nil
}

// postBytes performs a raw POST with an arbitrary byte body (used for
// restore, which uploads a database file rather than JSON).
func (c *Client) postBytes(path string, data []byte) error {
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.token != "" {
		req.Header.Set("X-Api-Key", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &APIError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("POST %s: %s", path, e.Error)}
	}
	return nil
}

// DownloadBackup triggers a manual backup and returns its bytes and the
// server-suggested filename ("<vtl_name>_<timestamp>.db").
func (c *Client) DownloadBackup() ([]byte, string, error) {
	return c.getBytes("/api/v1/backup/download")
}

// ListBackups lists previously scheduled backup files.
func (c *Client) ListBackups() ([]BackupFileInfo, error) {
	var out []BackupFileInfo
	err := c.do(http.MethodGet, "/api/v1/backups", nil, &out)
	return out, err
}

// DownloadStoredBackup downloads one previously scheduled backup file by name.
func (c *Client) DownloadStoredBackup(filename string) ([]byte, error) {
	data, _, err := c.getBytes("/api/v1/backups/" + filename + "/download")
	return data, err
}

// DeleteBackup removes one stored backup file.
func (c *Client) DeleteBackup(filename string) error {
	return c.do(http.MethodDelete, "/api/v1/backups/"+filename, nil, nil)
}

// GetBackupSchedule returns the current scheduled-backup configuration.
func (c *Client) GetBackupSchedule() (BackupScheduleInfo, error) {
	var out BackupScheduleInfo
	err := c.do(http.MethodGet, "/api/v1/backup/schedule", nil, &out)
	return out, err
}

// UpdateBackupSchedule sets the scheduled-backup interval/retention. A nil
// pointer leaves that field unchanged.
func (c *Client) UpdateBackupSchedule(interval *string, retention *int) (BackupScheduleInfo, error) {
	var out BackupScheduleInfo
	req := map[string]any{}
	if interval != nil {
		req["interval"] = *interval
	}
	if retention != nil {
		req["retention"] = *retention
	}
	err := c.do(http.MethodPut, "/api/v1/backup/schedule", req, &out)
	return out, err
}

// Restore uploads data (a previously downloaded backup file) and replaces
// the live database with it. On success the daemon restarts itself to
// apply the change - callers should expect the connection/next request to
// briefly fail while that happens.
func (c *Client) Restore(data []byte) error {
	return c.postBytes("/api/v1/restore", data)
}

// Reset wipes the entire VTL back to factory defaults - topology,
// settings, dynamic state, user accounts and API tokens - gated on
// confirmName matching the VTL's current name (or "RESET" if it has none
// yet). deleteVolumes additionally deletes every cartridge's backing file
// on disk. On success the daemon restarts itself, same as Restore.
func (c *Client) Reset(confirmName string, deleteVolumes bool) error {
	req := map[string]any{"confirm_name": confirmName, "delete_volumes": deleteVolumes}
	return c.do(http.MethodPost, "/api/v1/reset", req, nil)
}
