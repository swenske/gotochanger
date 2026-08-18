package offsite

import (
	"fmt"
	"strings"
	"time"

	"github.com/swenske/gotochanger/internal/mailer"
)

// renderRecapEmail builds the D-1 recap email content for a scheduled
// pickup visit, from whatever's currently pending for this mode - shared
// by ScheduledPickupMode.Tick's actual send and RecapPreview, so the
// preview can never drift from what a real send would contain.
func renderRecapEmail(eng *Engine, mode string, cfg ScheduledPickupConfig, due time.Time) mailer.Message {
	pickups, returns := eng.lib.OffsitePendingByMode(mode)
	var pickupBarcodes, returnBarcodes []string
	for _, v := range pickups {
		if v.Offsite != nil && v.Offsite.DueAt.Equal(due) {
			pickupBarcodes = append(pickupBarcodes, v.Barcode)
		}
	}
	for _, v := range returns {
		if v.Offsite != nil && v.Offsite.DueAt.Equal(due) {
			returnBarcodes = append(returnBarcodes, v.Barcode)
		}
	}

	weekday := due.Weekday().String()
	dateStr := due.Format("2006-01-02")
	subject := fmt.Sprintf("[gotochanger] Offsite vendor visit reminder - %s %s, %s-%s",
		weekday, dateStr, cfg.WindowStart, cfg.WindowEnd)

	var b strings.Builder
	fmt.Fprintf(&b, "Vendor visit scheduled for %s, %s between %s and %s.\n", weekday, dateStr, cfg.WindowStart, cfg.WindowEnd)
	if len(cfg.MailboxIDs) > 0 {
		fmt.Fprintf(&b, "Mailbox(es): %s\n", strings.Join(cfg.MailboxIDs, ", "))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Tapes to be picked up (%d):\n", len(pickupBarcodes))
	for _, bc := range pickupBarcodes {
		fmt.Fprintf(&b, "  - %s\n", bc)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Tapes to be returned (%d):\n", len(returnBarcodes))
	for _, bc := range returnBarcodes {
		fmt.Fprintf(&b, "  - %s\n", bc)
	}
	b.WriteString("\nThis is an automated message from gotochanger.\n")

	return mailer.Message{
		To:      cfg.RecapRecipients,
		Subject: subject,
		Body:    b.String(),
	}
}

func sendRecapEmail(eng *Engine, mode string, cfg ScheduledPickupConfig, due time.Time) error {
	return eng.mailer.Send(renderRecapEmail(eng, mode, cfg, due))
}
