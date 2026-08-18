// Package offsite implements the pluggable offsite-storage mode
// architecture: OffsiteMode is the plugin interface, Registry holds every
// registered mode, and Engine is what each mode's methods use to act on
// the library and record movement history without duplicating that logic
// per mode. Two modes are registered by cmd/gotochangerd: continuous
// rotation (today's pre-existing rotation behavior, wrapped) and scheduled
// pickup (the vendor-visit workflow). Adding a third mode later means
// implementing OffsiteMode and registering it - no API/UI code needs to
// change, since ParameterSchema lets the generic Admin UI render a config
// form and the generic API layer validate mailbox references without any
// mode-specific code outside this package.
package offsite

import (
	"context"
	"encoding/json"
	"time"
)

type OffsiteMode interface {
	// Name is the mode's stable id: its settings key and its wire
	// identifier in the API ("continuous_rotation", "scheduled_pickup").
	Name() string
	Label() string
	Describe() string
	ParameterSchema() []ParameterField
	DefaultConfig() json.RawMessage
	ValidateConfig(raw json.RawMessage) error

	RequestPickup(ctx context.Context, eng *Engine, barcodes []string, actor string) []ItemResult
	RequestReturn(ctx context.Context, eng *Engine, barcodes []string, urgent bool, actor string) []ItemResult
	// Tick is called on whichever mode is currently active (see
	// cmd/gotochangerd's offsite scheduler goroutine) to complete due
	// transfers and run any of the mode's own scheduled behavior (e.g.
	// Scheduled Pickup's D-1 recap email).
	Tick(ctx context.Context, eng *Engine, now time.Time) error
}

// ParameterField declaratively describes one of a mode's config fields, so
// the Admin UI can render a form and the API layer can validate
// ParameterTypeMailboxes references against live topology, both without
// any mode-specific code.
type ParameterField struct {
	Key         string        `json:"key"`
	Label       string        `json:"label"`
	Help        string        `json:"help,omitempty"`
	Placeholder string        `json:"placeholder,omitempty"`
	Type        ParameterType `json:"type"`
	Required    bool          `json:"required,omitempty"`
}

type ParameterType string

const (
	ParameterTypeString    ParameterType = "string"
	ParameterTypeInt       ParameterType = "int"
	ParameterTypeDuration  ParameterType = "duration"    // config.ParseDuration shape, e.g. "4h"
	ParameterTypeWeekday   ParameterType = "weekday"     // "monday".."sunday"
	ParameterTypeTimeOfDay ParameterType = "time_of_day" // "HH:MM"
	ParameterTypeBool      ParameterType = "bool"
	ParameterTypeEmailList ParameterType = "email_list"
	ParameterTypeMailboxes ParameterType = "mailboxes" // multi-select; existence-checked by the API layer, not the mode
)

// ItemResult is one barcode's outcome from RequestPickup/RequestReturn.
// The pickup/retrieval JSON payload endpoint is best-effort/partial-
// success (see internal/api/offsite_modes.go), so every barcode gets its
// own result rather than the whole request failing on the first bad one.
type ItemResult struct {
	Barcode string     `json:"barcode"`
	OK      bool       `json:"ok"`
	Error   string     `json:"error,omitempty"`
	DueAt   *time.Time `json:"due_at,omitempty"`
	// RequestID is the batch request this barcode joined, set only when
	// OK is true - a partially-failed batch has some items in the batch
	// and some not, which is the correct membership semantics (see
	// Engine.RequestPickupBatch/RequestReturnBatch).
	RequestID string `json:"request_id,omitempty"`
}

// unsupportedResults builds a uniform failure ItemResult for every
// barcode - used by modes (or mode states) that don't support the
// requested action at all, e.g. Continuous Rotation's RequestPickup/
// RequestReturn, or Scheduled Pickup acting before it has a saved config.
func unsupportedResults(barcodes []string, msg string) []ItemResult {
	out := make([]ItemResult, len(barcodes))
	for i, bc := range barcodes {
		out[i] = ItemResult{Barcode: bc, OK: false, Error: msg}
	}
	return out
}
