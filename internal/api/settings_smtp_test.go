package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/swenske/gotochanger/internal/config"
	"github.com/swenske/gotochanger/internal/mailer"
)

// TestSettingsSMTPPasswordNeverSerialized guards config.SMTPConfig's
// json:"-" Password tag at the point that actually matters: a real
// marshal of what GET/PUT /api/v1/settings return, not just the
// in-memory struct field (which of course always holds it - the daemon
// needs it in a form it can actually authenticate with).
func TestSettingsSMTPPasswordNeverSerialized(t *testing.T) {
	st := newSettingsTestStore(t)
	settings := NewSettings(config.Default(), nil, nil, nil, nil, st)

	password := "super-secret-password"
	result, err := settings.Update(UpdateSettingsRequest{SMTPPassword: &password})
	if err != nil {
		t.Fatalf("update settings: %v", err)
	}
	if !result.Config.SMTP.PasswordConfigured {
		t.Fatal("expected PasswordConfigured to be true after setting a password")
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal update result: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatalf("expected the raw password to never appear in a marshaled settings result, got: %s", raw)
	}
	if !strings.Contains(string(raw), `"password_configured":true`) {
		t.Fatalf("expected password_configured:true in the marshaled result, got: %s", raw)
	}

	raw, err = json.Marshal(settings.Current())
	if err != nil {
		t.Fatalf("marshal current config: %v", err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatalf("expected the raw password to never appear in a marshaled Current() config, got: %s", raw)
	}
}

// TestSettingsSMTPBlankPasswordPreservesExisting is the SMTP analogue of
// the mailbox PIN dialog's "blank means leave unchanged" contract: the
// Settings page submits every field on every save and the password input
// always starts blank (GET never returns it), so treating blank as
// "clear" would silently wipe a working password the next time an admin
// saved an unrelated field.
func TestSettingsSMTPBlankPasswordPreservesExisting(t *testing.T) {
	st := newSettingsTestStore(t)
	settings := NewSettings(config.Default(), nil, nil, nil, nil, st)

	password := "keep-me"
	if _, err := settings.Update(UpdateSettingsRequest{SMTPPassword: &password}); err != nil {
		t.Fatalf("set initial password: %v", err)
	}

	host := "smtp.example.com"
	result, err := settings.Update(UpdateSettingsRequest{SMTPHost: &host})
	if err != nil {
		t.Fatalf("unrelated update: %v", err)
	}
	if !result.Config.SMTP.PasswordConfigured {
		t.Fatal("expected PasswordConfigured to remain true after an unrelated update")
	}
	if got, _, _ := st.GetSetting("smtp_password"); got != password {
		t.Fatalf("smtp_password = %q after an unrelated update, want %q (preserved)", got, password)
	}

	// An explicit empty-string request (not just an omitted/nil field) must
	// be treated the same way - Update's guard is
	// `req.SMTPPassword != nil && *req.SMTPPassword != ""`, not just a nil
	// check.
	blank := ""
	result, err = settings.Update(UpdateSettingsRequest{SMTPPassword: &blank})
	if err != nil {
		t.Fatalf("blank-password update: %v", err)
	}
	if !result.Config.SMTP.PasswordConfigured {
		t.Fatal("expected PasswordConfigured to remain true after an explicit blank-string password update")
	}
	if got, _, _ := st.GetSetting("smtp_password"); got != password {
		t.Fatalf("smtp_password = %q after an explicit blank-string update, want %q (preserved)", got, password)
	}
}

// TestSettingsSMTPUpdateHotReloadsMailer guards the requirement that
// SMTPMailer mirror internal/snmp.Sender's hot-reload shape:
// restartRequiredFields' own doc comment promises every
// UpdateSettingsRequest field applies without a daemon restart, so a
// *mailer.SMTPMailer handed to NewSettings must see SetConfig called on
// every Update, not just be seeded once at construction.
func TestSettingsSMTPUpdateHotReloadsMailer(t *testing.T) {
	st := newSettingsTestStore(t)
	m := mailer.NewSMTPMailer(config.SMTPConfig{})
	settings := NewSettings(config.Default(), nil, nil, m, nil, st)

	enabled := true
	host := "smtp.example.com"
	port := 2525
	if _, err := settings.Update(UpdateSettingsRequest{SMTPEnabled: &enabled, SMTPHost: &host, SMTPPort: &port}); err != nil {
		t.Fatalf("update settings: %v", err)
	}

	got := m.Config()
	if !got.Enabled || got.Host != host || got.Port != port {
		t.Fatalf("expected Settings.Update to hot-reload the SMTPMailer's config, got %+v", got)
	}
}
