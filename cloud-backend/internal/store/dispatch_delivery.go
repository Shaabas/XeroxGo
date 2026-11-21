package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// RecordDispatchDeliveryFailure records transport uncertainty without changing
// order state, refund eligibility or the existing durable dispatch intent.
// A WebSocket error does not establish whether the Pi received the command.
func (s *Store) RecordDispatchDeliveryFailure(ctx context.Context, jobID, reason string) error {
	if s == nil || s.db == nil { return errors.New("store not initialized") }
	jobID = strings.TrimSpace(jobID)
	if jobID == "" { return errors.New("jobID required") }
	reason = strings.TrimSpace(reason)
	if len(reason) > 240 { reason = reason[:240] }
	_, err := s.db.ExecContext(ctx, s.rebind(`
UPDATE dispatch_intents
SET last_error = ?, retry_count = retry_count + 1, updated_at_ns = ?
WHERE job_id = ? AND status IN ('PENDING','SENT');`),
		reason, time.Now().UTC().UnixNano(), jobID)
	return err
}


// A WebSocket ack belongs to the authenticated kiosk connection, not merely
// to an attacker- or peer-supplied job_id. A mismatched kiosk cannot suppress
// this order's pending replay.
func (s *Store) MarkDispatchAcknowledgedForKiosk(ctx context.Context, kioskID, jobID string) error {
	if s == nil || s.db == nil { return errors.New("store not initialized") }
	kioskID = strings.TrimSpace(kioskID)
	jobID = strings.TrimSpace(jobID)
	if kioskID == "" || jobID == "" { return errors.New("kioskID and jobID required") }
	_, err := s.db.ExecContext(ctx, s.rebind(`
UPDATE dispatch_intents
SET status='ACKNOWLEDGED',acknowledged_at_ns=?,updated_at_ns=?
WHERE kiosk_id=? AND job_id=? AND status IN ('PENDING','SENT');`),
		time.Now().UTC().UnixNano(),time.Now().UTC().UnixNano(),kioskID,jobID)
	return err
}
