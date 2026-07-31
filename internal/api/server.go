package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/library"
	"github.com/swenske/gotochanger/internal/store"
)

// TopologyStore is everything the API layer needs from the SQLite-backed
// topology store (internal/store/topology.go) to serve the wizard and the
// Admin CRUD endpoints for drive types, tape types, magazines, drive
// devices, logical libraries, tape sets, and the related singleton
// settings. Defined here (rather than imported as a concrete type) so this
// package doesn't need to import internal/store directly; *store.Store
// satisfies it.
type TopologyStore interface {
	LoadTopology() (config.LibraryConfig, error)

	ListDriveTypes() ([]config.DriveType, error)
	CreateDriveType(config.DriveType) error
	UpdateDriveType(name string, dt config.DriveType) error
	DeleteDriveType(name string) error

	ListTapeTypes() ([]config.TapeType, error)
	CreateTapeType(config.TapeType) error
	UpdateTapeType(name string, tt config.TapeType) error
	DeleteTapeType(name string) error

	ListMagazines() ([]config.MagazineConfig, error)
	CreateMagazine(config.MagazineConfig) error
	UpdateMagazine(id string, m config.MagazineConfig) error
	DeleteMagazine(id string) error
	SaveMagazines([]config.MagazineConfig) error

	ListMailboxes() ([]config.MailboxConfig, error)
	CreateMailbox(config.MailboxConfig) error
	UpdateMailbox(id string, m config.MailboxConfig) error
	DeleteMailbox(id string) error
	SaveMailboxes([]config.MailboxConfig) error

	ListDriveDevices() ([]config.DriveDeviceConfig, error)
	SaveDriveDevices([]config.DriveDeviceConfig) error

	ListLogicalLibraries() ([]config.LogicalLibraryConfig, error)
	CreateLogicalLibrary(config.LogicalLibraryConfig) error
	UpdateLogicalLibrary(name string, lib config.LogicalLibraryConfig) error
	DeleteLogicalLibrary(name string) error
	SaveLogicalLibraries([]config.LogicalLibraryConfig) error

	ListTapeSets() ([]config.TapeSetConfig, error)
	CreateTapeSet(config.TapeSetConfig) error
	UpdateTapeSet(name string, ts config.TapeSetConfig) error
	DeleteTapeSet(name string) error
	SaveTapeSets([]config.TapeSetConfig) error

	GetSetting(key string) (string, bool, error)
	SetSetting(key, value string) error
	SetSNMPTargets(targets []config.SNMPTarget) error
	GetLatencySettings() (config.LatencySettings, error)
	SetLatencySettings(ls config.LatencySettings) error
	GetCleaningSettings() (config.CleaningSettings, error)
	SetCleaningSettings(cs config.CleaningSettings) error
}

// BackupStore is everything the API layer needs from *store.Store to serve
// the backup/restore/reset endpoints (internal/api/backup.go,
// internal/api/reset.go). Defined as an interface, like TopologyStore, so
// this package doesn't need to import internal/store directly.
type BackupStore interface {
	Path() string
	CreateBackupFile(backupsDir, vtlName string) (string, error)
	ListBackupFiles(backupsDir string) ([]store.BackupFileInfo, error)
	DeleteBackupFile(backupsDir, name string) error
	BackupFilePath(backupsDir, name string) (string, error)
	Restore(tmpPath string) error
	ResetToFactory() error
}

// Server holds everything needed to build the HTTP handler tree shared by
// both the authenticated TCP listener and the trusted Unix socket listener.
type Server struct {
	lib         *library.Library
	tokens      *TokenStore
	users       *UserStore
	sessions    *SessionStore
	settings    *Settings
	log         *slog.Logger
	cfg         config.Config
	wizardState WizardState
	persist     library.Persister
	topology    TopologyStore
	backup      BackupStore
	backupsDir  string
	restartFunc func()
	version     string
	broadcaster *Broadcaster
	mu          sync.RWMutex
	// kernelModeDevices holds the real SCSI device paths each running
	// gotochanger-tcmud instance has reported for itself, keyed by
	// logical library name ("default" for the whole-physical-library,
	// unscoped instance - see kernel_mode_devices.go). In-memory only,
	// deliberately not persisted: these are kernel-assigned identifiers
	// tied to a specific running process, meaningless across a restart.
	kernelModeDevices map[string]KernelModeDeviceReport
	startedAt         time.Time
	pm                *prometheusMetrics
}

// New builds a Server. lib, tokens, users and sessions must be non-nil; log
// may be nil to use slog.Default().
func New(lib *library.Library, tokens *TokenStore, users *UserStore, sessions *SessionStore, settings *Settings, cfg config.Config, log *slog.Logger, persist library.Persister, topology TopologyStore, backup BackupStore, backupsDir string) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{lib: lib, tokens: tokens, users: users, sessions: sessions, settings: settings, cfg: cfg, log: log, persist: persist, topology: topology, backup: backup, backupsDir: backupsDir, kernelModeDevices: map[string]KernelModeDeviceReport{}, startedAt: time.Now(), pm: newPrometheusMetrics()}
	if topology != nil {
		s.wizardState = loadWizardState(topology)
	}
	return s
}

// SetRestartFunc registers the callback Restore uses to bring the daemon
// back up against the newly-restored database (see internal/api/backup.go
// and store.Store.Restore's doc comment). Optional - if never set, a
// successful restore still swaps the database file but the caller is on
// their own for getting the daemon to pick it up.
func (s *Server) SetRestartFunc(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restartFunc = fn
}

// SetVersion records the build version (see cmd/gotochangerd's "-X
// main.version=..." ldflag) so it can be surfaced to the web UI, e.g. via
// authStateResponse. Optional - an unset version is simply omitted.
func (s *Server) SetVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
}

// SetBroadcaster registers the live-update broadcaster backing
// GET /api/v1/stream (see stream.go). Optional - if never set, the stream
// endpoint responds with 503 rather than hanging forever with no way to
// ever notify a subscriber.
func (s *Server) SetBroadcaster(b *Broadcaster) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broadcaster = b
}

// PublicHandler returns the authenticated handler tree (session cookie or
// API token), meant to be served on the TCP listener (remote/UI access).
func (s *Server) PublicHandler() http.Handler {
	mux := s.routes()
	return s.authenticate(s.audit(s.metricsMiddleware(mux)))
}

// TrustedHandler returns the handler tree with every request treated as an
// implicitly trusted local admin, meant to be served only on the local Unix
// domain socket, whose filesystem permissions already restrict access to
// trusted local users/processes (e.g. root, the bareos user, members of the
// gotochanger group).
func (s *Server) TrustedHandler() http.Handler {
	mux := s.routes()
	audited := s.audit(s.metricsMiddleware(mux))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := Principal{Subject: "trusted-socket", Role: RoleAdmin, Via: "trusted"}
		audited.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

func (s *Server) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/v1/stream's handler blocks for the connection's entire
		// lifetime (potentially hours) rather than returning promptly like
		// every other route, so annotateRequestEvents would only run once
		// the tab disconnects - and would then backfill actor/source onto
		// any event emitted anywhere during that whole window that still
		// had empty fields, misattributing unrelated events to whoever
		// happened to have the tab open. Exclude it from post-hoc
		// annotation entirely; nothing about opening the stream itself is
		// an auditable action.
		if r.URL.Path == "/api/v1/stream" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now().UTC()
		next.ServeHTTP(w, r)
		s.annotateRequestEvents(r, started)
	})
}

func (s *Server) annotateRequestEvents(r *http.Request, started time.Time) {
	if s == nil || s.lib == nil || r == nil {
		return
	}
	ip := sourceIPFromRequest(r)
	detail := map[string]string{
		"method":    r.Method,
		"path":      r.URL.Path,
		"source_ip": ip,
	}
	actor := ""
	source := "anonymous"
	if p, ok := principalFrom(r); ok {
		actor = p.Subject
		source = p.Via
		detail["username"] = actor
	}
	s.lib.AnnotateEventsSince(started, actor, source, detail)
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Public: no authentication required (this is how a client obtains one).
	mux.HandleFunc("GET /api/v1/auth/state", s.handleAuthState)
	mux.HandleFunc("POST /api/v1/auth/bootstrap", s.handleAuthBootstrap)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleAuthLogin)
	mux.HandleFunc("GET /api/v1/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /api/v1/snmp/mib", requireRole(RoleViewer, s.handleSNMPMIB))

	// Setup wizard. Admin-only, like every other endpoint that writes
	// topology (magazines, drives, logical libraries) - the wizard writes
	// all of it at once and hot-applies it, so anything weaker would be a
	// hole around the Admin gate those endpoints already have.
	//
	// These were previously registered with no role requirement at all,
	// commented "public for initial setup". That was never actually needed:
	// the web UI only ever calls them from enterAppOrWizard (app.js), which
	// runs after /api/v1/auth/state reports authenticated, and both
	// bootstrap and login establish an Admin session before that (see
	// handleAuthBootstrap's startSession call). Nothing revoked the
	// exemption once setup finished either, so on a configured, running
	// deployment any unauthenticated caller that could reach the TCP
	// listener could read the full topology and rewrite it. gotochangerctl
	// and the Bareos shim are unaffected: the trusted Unix socket already
	// presents every request as admin.
	mux.HandleFunc("GET /api/v1/wizard", requireRole(RoleAdmin, s.handleGetWizard))
	mux.HandleFunc("POST /api/v1/wizard", requireRole(RoleAdmin, s.handleUpdateWizard))
	mux.HandleFunc("POST /api/v1/wizard/complete", requireRole(RoleAdmin, s.handleCompleteWizard))
	mux.HandleFunc("POST /api/v1/wizard/reset", requireRole(RoleAdmin, s.handleResetWizard))
	mux.HandleFunc("GET /api/v1/wizard/options", requireRole(RoleAdmin, s.handleGetWizardOptions))

	// Any authenticated principal (viewer and above).
	mux.HandleFunc("POST /api/v1/auth/logout", requireRole(RoleViewer, s.handleAuthLogout))
	mux.HandleFunc("POST /api/v1/auth/change-password", requireRole(RoleViewer, s.handleChangePassword))

	mux.HandleFunc("GET /api/v1/kernel-mode/status", requireRole(RoleViewer, s.handleKernelModeStatus))
	mux.HandleFunc("GET /api/v1/kernel-mode/devices", requireRole(RoleViewer, s.handleListKernelModeDevices))
	mux.HandleFunc("POST /api/v1/kernel-mode/devices/{instance}", requireRole(RoleOperator, s.handleReportKernelModeDevices))
	mux.HandleFunc("DELETE /api/v1/kernel-mode/devices/{instance}", requireRole(RoleOperator, s.handleClearKernelModeDevices))
	mux.HandleFunc("GET /api/v1/status", requireRole(RoleViewer, s.handleStatus))
	mux.HandleFunc("GET /api/v1/events", requireRole(RoleViewer, s.handleEvents))
	mux.HandleFunc("GET /api/v1/stream", requireRole(RoleViewer, s.handleStream))
	mux.HandleFunc("GET /api/v1/volumes", requireRole(RoleViewer, s.handleListVolumes))
	mux.HandleFunc("GET /api/v1/outside", requireRole(RoleViewer, s.handleListOutsideVolumes))
	mux.HandleFunc("GET /api/v1/cleaning/tapes", requireRole(RoleViewer, s.handleListCleaningTapes))

	// Operator and above: everything that operates the simulated library.
	mux.HandleFunc("DELETE /api/v1/outside/{barcode}", requireRole(RoleOperator, s.handleDeleteOutsideVolume))
	mux.HandleFunc("POST /api/v1/tape-sets/{name}/tapes", requireRole(RoleOperator, s.handleAddTapeSetTapes))
	mux.HandleFunc("POST /api/v1/cleaning/tapes", requireRole(RoleOperator, s.handleCreateCleaningTape))
	mux.HandleFunc("POST /api/v1/doors/io/{id}/open", requireRole(RoleOperator, s.handleOpenIODoor))
	mux.HandleFunc("POST /api/v1/doors/io/{id}/close", requireRole(RoleOperator, s.handleCloseIODoor))
	mux.HandleFunc("POST /api/v1/doors/storage/{id}/open", requireRole(RoleOperator, s.handleOpenStorageDoor))
	mux.HandleFunc("POST /api/v1/doors/storage/{id}/close", requireRole(RoleOperator, s.handleCloseStorageDoor))
	mux.HandleFunc("POST /api/v1/load", requireRole(RoleOperator, s.handleLoad))
	mux.HandleFunc("POST /api/v1/unload", requireRole(RoleOperator, s.handleUnload))
	mux.HandleFunc("POST /api/v1/move", requireRole(RoleOperator, s.handleMove))
	mux.HandleFunc("POST /api/v1/drives/{index}/fault", requireRole(RoleOperator, s.handleDriveFault))
	mux.HandleFunc("POST /api/v1/robotics/fault", requireRole(RoleOperator, s.handleRoboticFault))
	mux.HandleFunc("POST /api/v1/volumes/{barcode}/write-protect", requireRole(RoleOperator, s.handleSetVolumeWriteProtect))

	// Admin only: user management, token management, application settings.
	mux.HandleFunc("GET /api/v1/users", requireRole(RoleAdmin, s.handleListUsers))
	mux.HandleFunc("POST /api/v1/users", requireRole(RoleAdmin, s.handleCreateUser))
	mux.HandleFunc("DELETE /api/v1/users/{username}", requireRole(RoleAdmin, s.handleDeleteUser))
	mux.HandleFunc("POST /api/v1/users/{username}/role", requireRole(RoleAdmin, s.handleSetUserRole))
	mux.HandleFunc("POST /api/v1/users/{username}/reset-password", requireRole(RoleAdmin, s.handleResetUserPassword))

	mux.HandleFunc("GET /api/v1/tokens", requireRole(RoleAdmin, s.handleListTokens))
	mux.HandleFunc("POST /api/v1/tokens", requireRole(RoleAdmin, s.handleCreateToken))
	mux.HandleFunc("DELETE /api/v1/tokens/{name}", requireRole(RoleAdmin, s.handleRevokeToken))

	mux.HandleFunc("GET /api/v1/settings", requireRole(RoleAdmin, s.handleGetSettings))
	mux.HandleFunc("PUT /api/v1/settings", requireRole(RoleAdmin, s.handleUpdateSettings))
	mux.HandleFunc("GET /api/v1/settings/latency", requireRole(RoleAdmin, s.handleGetLatencySettings))
	mux.HandleFunc("PUT /api/v1/settings/latency", requireRole(RoleAdmin, s.handleUpdateLatencySettings))
	mux.HandleFunc("GET /api/v1/settings/cleaning", requireRole(RoleAdmin, s.handleGetCleaningSettings))
	mux.HandleFunc("PUT /api/v1/settings/cleaning", requireRole(RoleAdmin, s.handleUpdateCleaningSettings))
	mux.HandleFunc("GET /api/v1/settings/prometheus", requireRole(RoleAdmin, s.handleGetPrometheusSettings))
	mux.HandleFunc("PUT /api/v1/settings/prometheus", requireRole(RoleAdmin, s.handleUpdatePrometheusSettings))
	mux.HandleFunc("GET /api/v1/prometheus/dashboard", requireRole(RoleAdmin, s.handleDownloadGrafanaDashboard))
	mux.HandleFunc("GET /api/v1/settings/pin", requireRole(RoleAdmin, s.handleGetPINSettings))
	mux.HandleFunc("PUT /api/v1/settings/pin", requireRole(RoleAdmin, s.handleUpdatePINSettings))

	// Backup/restore - Admin-only throughout, including the manual
	// "download now" trigger.
	//
	// That last one used to be Operator+, per the original spec ("an
	// operator can trigger and download a manual backup"), on the
	// then-correct reasoning that a backup holds only virtual-tape-library
	// state: user accounts and API tokens lived in separate users.json/
	// tokens.json files and were deliberately excluded. That stopped being
	// true when auth moved into SQLite - the users and tokens tables now
	// live in the same state.db, and a backup is a VACUUM INTO of the whole
	// file, so an operator-downloadable backup hands out every admin's
	// PBKDF2 password hash for offline cracking.
	//
	// Stripping those two tables from the operator's copy was considered
	// and rejected: a stripped database restored later would come back with
	// an empty users table, which puts the daemon back into "bootstrap
	// required" - i.e. whoever reaches the web UI first sets the new admin
	// password. Keeping backups complete and the endpoint Admin-only trades
	// one operator convenience for both a smaller blast radius and a
	// restore that still round-trips exactly.
	mux.HandleFunc("GET /api/v1/backup/download", requireRole(RoleAdmin, s.handleBackupDownload))
	mux.HandleFunc("GET /api/v1/backup/schedule", requireRole(RoleAdmin, s.handleGetBackupSchedule))
	mux.HandleFunc("PUT /api/v1/backup/schedule", requireRole(RoleAdmin, s.handleUpdateBackupSchedule))
	mux.HandleFunc("GET /api/v1/backups", requireRole(RoleAdmin, s.handleListBackups))
	mux.HandleFunc("GET /api/v1/backups/{filename}/download", requireRole(RoleAdmin, s.handleDownloadStoredBackup))
	mux.HandleFunc("DELETE /api/v1/backups/{filename}", requireRole(RoleAdmin, s.handleDeleteStoredBackup))
	mux.HandleFunc("POST /api/v1/restore", requireRole(RoleAdmin, s.handleRestore))
	mux.HandleFunc("POST /api/v1/reset", requireRole(RoleAdmin, s.handleReset))

	// Logical library management
	mux.HandleFunc("GET /api/v1/logical-libraries", requireRole(RoleAdmin, s.handleListLogicalLibraries))
	mux.HandleFunc("GET /api/v1/logical-libraries/{name}", requireRole(RoleAdmin, s.handleGetLogicalLibrary))
	mux.HandleFunc("POST /api/v1/logical-libraries", requireRole(RoleAdmin, s.handleCreateLogicalLibrary))
	mux.HandleFunc("PUT /api/v1/logical-libraries/{name}", requireRole(RoleAdmin, s.handleUpdateLogicalLibrary))
	mux.HandleFunc("DELETE /api/v1/logical-libraries/{name}", requireRole(RoleAdmin, s.handleDeleteLogicalLibrary))
	mux.HandleFunc("GET /api/v1/unassigned", requireRole(RoleAdmin, s.handleUnassignedElements))

	// Drive type catalog management
	mux.HandleFunc("GET /api/v1/drive-types", requireRole(RoleAdmin, s.handleListDriveTypes))
	mux.HandleFunc("POST /api/v1/drive-types", requireRole(RoleAdmin, s.handleCreateDriveType))
	mux.HandleFunc("PUT /api/v1/drive-types/{name}", requireRole(RoleAdmin, s.handleUpdateDriveType))
	mux.HandleFunc("DELETE /api/v1/drive-types/{name}", requireRole(RoleAdmin, s.handleDeleteDriveType))

	// Drive device management (physical drives, not the drive-type catalog)
	mux.HandleFunc("GET /api/v1/drives", requireRole(RoleAdmin, s.handleListDrives))
	mux.HandleFunc("POST /api/v1/drives", requireRole(RoleAdmin, s.handleCreateDrive))
	mux.HandleFunc("PUT /api/v1/drives/{index}", requireRole(RoleAdmin, s.handleUpdateDrive))
	mux.HandleFunc("DELETE /api/v1/drives/{index}", requireRole(RoleAdmin, s.handleDeleteDrive))

	// Tape type catalog management
	mux.HandleFunc("GET /api/v1/tape-types", requireRole(RoleAdmin, s.handleListTapeTypes))
	mux.HandleFunc("POST /api/v1/tape-types", requireRole(RoleAdmin, s.handleCreateTapeType))
	mux.HandleFunc("PUT /api/v1/tape-types/{name}", requireRole(RoleAdmin, s.handleUpdateTapeType))
	mux.HandleFunc("DELETE /api/v1/tape-types/{name}", requireRole(RoleAdmin, s.handleDeleteTapeType))

	// Tape set management
	mux.HandleFunc("GET /api/v1/tape-sets", requireRole(RoleAdmin, s.handleListTapeSets))
	mux.HandleFunc("POST /api/v1/tape-sets", requireRole(RoleAdmin, s.handleCreateTapeSet))
	mux.HandleFunc("PUT /api/v1/tape-sets/{name}", requireRole(RoleAdmin, s.handleUpdateTapeSet))
	mux.HandleFunc("DELETE /api/v1/tape-sets/{name}", requireRole(RoleAdmin, s.handleDeleteTapeSet))

	// Filesystem browsing, backing the Admin UI's tape-set storage-folder picker
	mux.HandleFunc("GET /api/v1/fs/browse", requireRole(RoleAdmin, s.handleFSBrowse))

	// Magazine management
	mux.HandleFunc("GET /api/v1/magazines", requireRole(RoleAdmin, s.handleListMagazines))
	mux.HandleFunc("POST /api/v1/magazines", requireRole(RoleAdmin, s.handleCreateMagazine))
	mux.HandleFunc("PUT /api/v1/magazines/{id}", requireRole(RoleAdmin, s.handleUpdateMagazine))
	mux.HandleFunc("DELETE /api/v1/magazines/{id}", requireRole(RoleAdmin, s.handleDeleteMagazine))

	// Mailbox management
	mux.HandleFunc("GET /api/v1/mailboxes", requireRole(RoleAdmin, s.handleListMailboxes))
	mux.HandleFunc("POST /api/v1/mailboxes", requireRole(RoleAdmin, s.handleCreateMailbox))
	mux.HandleFunc("PUT /api/v1/mailboxes/{id}", requireRole(RoleAdmin, s.handleUpdateMailbox))
	mux.HandleFunc("DELETE /api/v1/mailboxes/{id}", requireRole(RoleAdmin, s.handleDeleteMailbox))

	// Offsite vault (manual send/recall + status)
	mux.HandleFunc("GET /api/v1/offsite", requireRole(RoleViewer, s.handleListOffsiteVolumes))
	mux.HandleFunc("POST /api/v1/offsite/send", requireRole(RoleOperator, s.handleOffsiteSend))
	mux.HandleFunc("POST /api/v1/offsite/recall", requireRole(RoleOperator, s.handleOffsiteRecall))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Unauthenticated by design (standard Prometheus scrape practice, and
	// this ticket's explicit requirement) - same pattern as /healthz above.
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	RegisterWebUI(mux)
	RegisterSwaggerUI(mux)
	RegisterUserGuide(mux)
	return mux
}

func (s *Server) handleGetWizard(w http.ResponseWriter, r *http.Request) {
	state := s.GetWizardState()
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleUpdateWizard(w http.ResponseWriter, r *http.Request) {
	var req WizardRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	state, err := s.UpdateWizardState(req)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleCompleteWizard(w http.ResponseWriter, r *http.Request) {
	if err := s.CompleteWizard(); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResetWizard(w http.ResponseWriter, r *http.Request) {
	s.ResetWizard()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetWizardOptions(w http.ResponseWriter, r *http.Request) {
	options := s.GetWizardOptions()
	writeJSON(w, http.StatusOK, options)
}

// authenticate resolves a Principal from either the session cookie (used by
// the web UI) or an API token (used by scripts/automation), attaches it to
// the request context, and lets routes() enforce per-endpoint role
// requirements. Requests with no valid credential still proceed (as an
// anonymous, roleless principal is simply absent from the context); public
// endpoints and static assets don't call requireRole so they work either
// way, while requireRole-wrapped handlers reject the request with 401.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := s.principalFromSession(r); ok {
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
			return
		}
		if p, ok := s.principalFromToken(r); ok {
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) principalFromSession(r *http.Request) (Principal, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return Principal{}, false
	}
	username, ok := s.sessions.Username(c.Value)
	if !ok {
		return Principal{}, false
	}
	user, ok := s.users.Get(username)
	if !ok {
		return Principal{}, false
	}
	return Principal{Subject: user.Username, Role: user.Role, Via: "session"}, true
}

func (s *Server) principalFromToken(r *http.Request) (Principal, bool) {
	token := bearerToken(r)
	if token == "" {
		return Principal{}, false
	}
	role, ok := s.tokens.Verify(token)
	if !ok {
		return Principal{}, false
	}
	return Principal{Subject: "token", Role: role, Via: "token"}, true
}

func bearerToken(r *http.Request) string {
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return v
	}
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
}

// ---- request/response helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorResponse{Error: err.Error()})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, library.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, library.ErrInvalidBarcode), errors.Is(err, library.ErrInvalidTarget), errors.Is(err, library.ErrUnknownTapeSet), errors.Is(err, library.ErrInvalidRoboticFaultKind):
		return http.StatusBadRequest
	case errors.Is(err, library.ErrEmpty), errors.Is(err, library.ErrFull), errors.Is(err, library.ErrDriveFault), errors.Is(err, library.ErrRoboticFault), errors.Is(err, library.ErrDoorClosed), errors.Is(err, library.ErrOutsideOnly), errors.Is(err, library.ErrAlreadyExists), errors.Is(err, library.ErrBarcodeExists), errors.Is(err, library.ErrCleaningTapeExpired), errors.Is(err, library.ErrCleaningPoolFull), errors.Is(err, library.ErrCleaningTapeUnavailable), errors.Is(err, library.ErrOffsiteDisabled), errors.Is(err, library.ErrVolumeNotAccessible):
		return http.StatusConflict
	case errors.Is(err, library.ErrOutsideLogicalLibrary), errors.Is(err, library.ErrPINRequired), errors.Is(err, library.ErrInvalidPIN):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func elementRef(kind, addr string) (library.ElementRef, error) {
	var k library.Kind
	switch kind {
	case "slot":
		k = library.KindSlot
	case "ioslot":
		k = library.KindIOSlot
	case "drive":
		k = library.KindDrive
	default:
		return library.ElementRef{}, errors.New("kind must be one of slot, ioslot, drive")
	}
	n, err := parseInt(addr)
	if err != nil {
		return library.ElementRef{}, errors.New("address must be an integer")
	}
	return library.ElementRef{Kind: k, Address: n}, nil
}

func parseInt(s string) (int, error) {
	return strconv.Atoi(s)
}
