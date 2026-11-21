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
