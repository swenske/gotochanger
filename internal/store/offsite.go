package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// offsiteSchema is the offsite feature's own table, kept in its own file
// (unlike the rest of topology.go's tables) since it's a time-series audit
// log, not topology - still registered from initTopologySchema() like
// everything else, so VACUUM INTO backups cover it with no separate
// registration step.
const offsiteSchema = `
CREATE TABLE IF NOT EXISTS offsite_movements (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	time TEXT NOT NULL,
	barcode TEXT NOT NULL,
	direction TEXT NOT NULL,
	status TEXT NOT NULL,
	mode TEXT NOT NULL,
	mailbox_id TEXT NOT NULL DEFAULT '',
	actor TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS offsite_movements_barcode_idx ON offsite_movements(barcode);
CREATE INDEX IF NOT EXISTS offsite_movements_time_idx ON offsite_movements(time DESC);
CREATE TABLE IF NOT EXISTS offsite_requests (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	mode TEXT NOT NULL,
	urgent INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	actor TEXT NOT NULL DEFAULT '',
	cancelled_at TEXT
);
CREATE INDEX IF NOT EXISTS offsite_requests_created_at_idx ON offsite_requests(created_at);
CREATE TABLE IF NOT EXISTS offsite_request_barcodes (
	request_id TEXT NOT NULL REFERENCES offsite_requests(id),
	barcode TEXT NOT NULL,
	PRIMARY KEY (request_id, barcode)
);
`

func (s *Store) initOffsiteSchema() error {
	_, err := s.db.Exec(offsiteSchema)
	return err
}

// OffsiteMovement is one row of the offsite audit trail: a single
// pickup/vaulting/return-request/return event for one barcode. Direction
// is one of "pickup_requested"/"sent_offsite"/"return_requested"/
// "returned_onsite"; Status is the resulting OffsiteStatus (empty for
// "returned_onsite", since the volume no longer has an offsite record at
// all once it's back).
type OffsiteMovement struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Barcode   string    `json:"barcode"`
	Direction string    `json:"direction"`
	Status    string    `json:"status,omitempty"`
	Mode      string    `json:"mode"`
	MailboxID string    `json:"mailbox_id,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	// RequestID links this movement back to the offsite_requests row that
	// caused it - set on pickup_requested/return_requested (the request
	// itself), the *_cancelled rows a cancellation produces, and the
	// sent_offsite/returned_onsite completion that later resolves it
	// (read from the volume's own OffsiteInfo.RequestID before it's
	// cleared - see internal/offsite.Engine.CompleteDueTransfers). Empty
	// for movements with no batch request behind them at all (manual
	// send/recall, Continuous Rotation's automatic sends).
	RequestID string `json:"request_id,omitempty"`
}

// RecordOffsiteMovement appends one audit-trail row. Time defaults to now
// (UTC) if the caller leaves it zero.
func (s *Store) RecordOffsiteMovement(m OffsiteMovement) error {
	t := m.Time
	if t.IsZero() {
		t = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO offsite_movements (time, barcode, direction, status, mode, mailbox_id, actor, detail, request_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.UTC().Format(time.RFC3339), m.Barcode, m.Direction, m.Status, m.Mode, m.MailboxID, m.Actor, m.Detail, m.RequestID,
	)
	if err != nil {
		return fmt.Errorf("record offsite movement: %w", err)
	}
	return nil
}

// ListOffsiteMovements returns the most recent movements, newest first,
// optionally filtered to one barcode's history. limit<=0 defaults to 200.
func (s *Store) ListOffsiteMovements(barcode string, limit int) ([]OffsiteMovement, error) {
	if limit <= 0 {
		limit = 200
	}
	var rows *sql.Rows
	var err error
	if barcode == "" {
		rows, err = s.db.Query(
			`SELECT id, time, barcode, direction, status, mode, mailbox_id, actor, detail, request_id
			 FROM offsite_movements ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = s.db.Query(
			`SELECT id, time, barcode, direction, status, mode, mailbox_id, actor, detail, request_id
			 FROM offsite_movements WHERE barcode = ? ORDER BY id DESC LIMIT ?`, barcode, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list offsite movements: %w", err)
	}
	defer rows.Close()

	var out []OffsiteMovement
	for rows.Next() {
		var m OffsiteMovement
		var timeRaw string
		if err := rows.Scan(&m.ID, &timeRaw, &m.Barcode, &m.Direction, &m.Status, &m.Mode, &m.MailboxID, &m.Actor, &m.Detail, &m.RequestID); err != nil {
			return nil, fmt.Errorf("scan offsite movement: %w", err)
		}
		m.Time, _ = time.Parse(time.RFC3339, timeRaw)
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetOffsiteActiveMode/SetOffsiteActiveMode and GetOffsiteModeConfig/
// SetOffsiteModeConfig mirror SetSNMPTargets' "JSON blob in one settings
// key" pattern rather than the flat one-key-per-field pattern Latency/
// Cleaning use, since different modes have different, heterogeneous
// parameter shapes - one fixed Go struct/table per mode would defeat the
// point of a plugin architecture.

func (s *Store) GetOffsiteActiveMode() (string, error) {
	return s.getStringSetting("offsite_active_mode", "")
}

func (s *Store) SetOffsiteActiveMode(name string) error {
	return s.SetSetting("offsite_active_mode", name)
}

func (s *Store) GetOffsiteModeConfig(mode string) (json.RawMessage, bool, error) {
	v, ok, err := s.GetSetting("offsite_mode_config_" + mode)
	if err != nil || !ok || v == "" {
		return nil, false, err
	}
	return json.RawMessage(v), true, nil
}

func (s *Store) SetOffsiteModeConfig(mode string, raw json.RawMessage) error {
	return s.SetSetting("offsite_mode_config_"+mode, string(raw))
}

// OffsiteRequest is one pickup/return batch's identity: an immutable
// snapshot of what was originally requested (id, kind, mode, member
// barcodes, urgent, created_at, actor), plus one mutable field
// (CancelledAt) set only by CancelOffsiteRequestRecord. Never deleted -
// the same "keep the audit trail" convention offsite_movements already
// follows - so a request row survives even after every member barcode
// has completed or been cancelled; "is it still actionable" is derived
// live from Library state (see internal/offsite's Engine and
// internal/api's handleListOffsiteRequests), not stored here.
type OffsiteRequest struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"` // "pickup" | "return"
	Mode        string     `json:"mode"`
	Barcodes    []string   `json:"barcodes"`
	Urgent      bool       `json:"urgent,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Actor       string     `json:"actor,omitempty"`
	CancelledAt *time.Time `json:"cancelled_at,omitempty"`
}

// CreateOffsiteRequest persists a brand-new request and its barcode
// membership in one transaction - insert-only, mirroring
// CreateLogicalLibrary's shape (this table has no corresponding "Update",
// only the narrower CancelOffsiteRequestRecord below).
func (s *Store) CreateOffsiteRequest(req OffsiteRequest) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	urgent := 0
	if req.Urgent {
		urgent = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO offsite_requests (id, kind, mode, urgent, created_at, actor) VALUES (?, ?, ?, ?, ?, ?)`,
		req.ID, req.Kind, req.Mode, urgent, req.CreatedAt.UTC().Format(time.RFC3339), req.Actor,
	); err != nil {
		return fmt.Errorf("offsite request %s already exists or is invalid: %w", req.ID, err)
	}
	for _, bc := range req.Barcodes {
		if _, err := tx.Exec(`INSERT INTO offsite_request_barcodes (request_id, barcode) VALUES (?, ?)`, req.ID, bc); err != nil {
			return fmt.Errorf("link barcode %s to offsite request %s: %w", bc, req.ID, err)
		}
	}
	return tx.Commit()
}

func scanOffsiteRequest(scan func(dest ...any) error) (OffsiteRequest, error) {
	var req OffsiteRequest
	var createdAtRaw string
	var urgent int
	var cancelledAtRaw sql.NullString
	if err := scan(&req.ID, &req.Kind, &req.Mode, &urgent, &createdAtRaw, &req.Actor, &cancelledAtRaw); err != nil {
		return OffsiteRequest{}, err
	}
	req.Urgent = urgent != 0
	req.CreatedAt, _ = time.Parse(time.RFC3339, createdAtRaw)
	if cancelledAtRaw.Valid {
		t, _ := time.Parse(time.RFC3339, cancelledAtRaw.String)
		req.CancelledAt = &t
	}
	return req, nil
}

// GetOffsiteRequest looks up one request by id, hydrating its barcode
// membership from offsite_request_barcodes - mirrors
// ListLogicalLibraries' row-then-junction-query pattern. ok is false if
// no request with this id exists.
func (s *Store) GetOffsiteRequest(id string) (OffsiteRequest, bool, error) {
	row := s.db.QueryRow(
		`SELECT id, kind, mode, urgent, created_at, actor, cancelled_at FROM offsite_requests WHERE id = ?`, id)
	req, err := scanOffsiteRequest(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return OffsiteRequest{}, false, nil
	}
	if err != nil {
		return OffsiteRequest{}, false, fmt.Errorf("get offsite request %s: %w", id, err)
	}
	barcodes, err := s.queryStringColumn("SELECT barcode FROM offsite_request_barcodes WHERE request_id = ? ORDER BY barcode", id)
	if err != nil {
		return OffsiteRequest{}, false, err
	}
	req.Barcodes = barcodes
	return req, true, nil
}

// ListOffsiteRequests returns the most recent requests, newest first.
// limit<=0 defaults to 200, mirroring ListOffsiteMovements. Ordered by
// created_at (not id): id is a random hex string (see
// internal/offsite's newRequestID), not an autoincrementing int like
// offsite_movements.id, so it carries no chronological meaning of its
// own - see the offsite_requests_created_at_idx index this relies on.
func (s *Store) ListOffsiteRequests(limit int) ([]OffsiteRequest, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT id, kind, mode, urgent, created_at, actor, cancelled_at FROM offsite_requests ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list offsite requests: %w", err)
	}
	defer rows.Close()

	var out []OffsiteRequest
	for rows.Next() {
		req, err := scanOffsiteRequest(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan offsite request: %w", err)
		}
		out = append(out, req)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		barcodes, err := s.queryStringColumn("SELECT barcode FROM offsite_request_barcodes WHERE request_id = ? ORDER BY barcode", out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Barcodes = barcodes
	}
	return out, nil
}

// CancelOffsiteRequestRecord marks a request cancelled, guarded by "not
// already cancelled" at the SQL layer - a second, DB-level defense
// against a double-cancel race alongside internal/offsite's Engine's own
// pre-check via GetOffsiteRequest. Returns whether this call actually
// flipped the row (false if it was already cancelled, or id doesn't
// exist).
func (s *Store) CancelOffsiteRequestRecord(id string, cancelledAt time.Time) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE offsite_requests SET cancelled_at = ? WHERE id = ? AND cancelled_at IS NULL`,
		cancelledAt.UTC().Format(time.RFC3339), id,
	)
	if err != nil {
		return false, fmt.Errorf("cancel offsite request %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cancel offsite request %s: %w", id, err)
	}
	return n > 0, nil
}

// migrateOffsiteMovementsSchema adds the request_id column (introduced
// alongside cancelable, per-batch offsite requests) to a pre-existing
// offsite_movements table (idempotent) - mirrors migrateDriveDevicesSchema's
// exact shape. A pre-existing movement row simply has no request_id
// (empty string, matching actor/mailbox_id/detail's own NOT NULL DEFAULT
// ” convention in this same table) since it predates the concept.
func (s *Store) migrateOffsiteMovementsSchema() error {
	cols, err := s.tableColumns("offsite_movements")
	if err != nil {
		return fmt.Errorf("inspect offsite_movements schema: %w", err)
	}
	if !cols["request_id"] {
		if _, err := s.db.Exec(`ALTER TABLE offsite_movements ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add offsite_movements.request_id: %w", err)
		}
	}
	return nil
}

// migrateLegacyOffsiteRotationSetting folds the pre-mode-architecture
// offsite_rotation_interval/offsite_rotation_count settings into the new
// "continuous_rotation" mode's own config the first time this runs on a
// database that predates it, so an existing deployment's rotation
// schedule keeps working identically once the daemon upgrades. Reads the
// raw offsite_rotation_interval/offsite_rotation_count SQLite keys
// directly, never through a config.LibraryConfig field - those Go fields
// existed only until every mode's config moved into
// offsite_mode_config_* (see GetOffsiteModeConfig/SetOffsiteModeConfig
// above) and were removed once that migration shipped. Deliberately
// never deletes the old keys afterwards: leaving them in place is
// harmless (nothing else ever reads them again) and cheaper than adding
// a DELETE step this migration doesn't otherwise need.
// Guarded by "offsite_active_mode not yet present", exactly like
// migrateLegacyLatencySetting, so it only ever runs once per database -
// including the "nothing to migrate" case (no legacy interval set),
// which still records offsite_active_mode="" so this never re-runs.
func (s *Store) migrateLegacyOffsiteRotationSetting() error {
	if _, migrated, err := s.GetSetting("offsite_active_mode"); err != nil {
		return err
	} else if migrated {
		return nil
	}
	interval, _, err := s.GetSetting("offsite_rotation_interval")
	if err != nil {
		return err
	}
	if interval == "" {
		return s.SetSetting("offsite_active_mode", "")
	}
	countRaw, _, err := s.GetSetting("offsite_rotation_count")
	if err != nil {
		return err
	}
	count, _ := strconv.Atoi(countRaw)
	cfg := struct {
		Interval string `json:"interval"`
		Count    int    `json:"count"`
	}{interval, count}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal continuous_rotation config: %w", err)
	}
	if err := s.SetOffsiteModeConfig("continuous_rotation", raw); err != nil {
		return err
	}
	return s.SetOffsiteActiveMode("continuous_rotation")
}
