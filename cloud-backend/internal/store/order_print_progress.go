package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// OrderPrintProgress is the order-wide view of physical job progress.
// The job_progress table stores one row per document JobID; choosing only
// the parent/first JobID silently drops all subsequent documents.
type OrderPrintProgress struct {
	DocumentsTotal     int
	DocumentsCompleted int
	DocumentsPrinting  int
	DocumentsSpooling  int
	DocumentsPaused    int
	DocumentsFailed    int
	PagesPrinted       int
	SheetsPrinted      int
	ErrorReason        string
}

// GetOrderPrintProgress aggregates only child jobs belonging to this order.
// One SQL statement gives a consistent snapshot of document counts, physical
// progress, and the latest non-empty error for canonical child documents.
func (s *Store) GetOrderPrintProgress(ctx context.Context, orderID string) (OrderPrintProgress, error) {
	if s == nil || s.db == nil {
		return OrderPrintProgress{}, errors.New("store not initialized")
	}
	if ctx == nil {
		return OrderPrintProgress{}, errors.New("context is nil")
	}
	if err := ctx.Err(); err != nil {
		return OrderPrintProgress{}, err
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return OrderPrintProgress{}, errors.New("orderID cannot be empty")
	}
	const query = `
SELECT COUNT(d.job_id),
 COALESCE(SUM(CASE WHEN d.status = 'COMPLETED' THEN 1 ELSE 0 END), 0),
 COALESCE(SUM(CASE WHEN d.status = 'PRINTING' THEN 1 ELSE 0 END), 0),
 COALESCE(SUM(CASE WHEN d.status = 'SPOOLING' THEN 1 ELSE 0 END), 0),
 COALESCE(SUM(CASE WHEN d.status = 'PAUSED_ERROR' THEN 1 ELSE 0 END), 0),
 COALESCE(SUM(CASE WHEN d.status = 'FAILED_REFUNDABLE' THEN 1 ELSE 0 END), 0),
 COALESCE(SUM(p.pages_printed), 0),
 COALESCE(SUM(p.sheets_printed), 0),
 COALESCE((
   SELECT ep.error_reason
   FROM order_documents ed
   JOIN job_progress ep ON ep.job_id = ed.job_id
   WHERE ed.order_id = ? AND ep.error_reason <> ''
   ORDER BY ep.timestamp_ns DESC, ed.document_index ASC
   LIMIT 1
 ), '')
FROM order_documents d
LEFT JOIN job_progress p ON p.job_id = d.job_id
WHERE d.order_id = ?;`
	var out OrderPrintProgress
	err := s.db.QueryRowContext(ctx, s.rebind(query), orderID, orderID).Scan(
		&out.DocumentsTotal, &out.DocumentsCompleted,
		&out.DocumentsPrinting, &out.DocumentsSpooling,
		&out.DocumentsPaused, &out.DocumentsFailed,
		&out.PagesPrinted, &out.SheetsPrinted, &out.ErrorReason,
	)
	if err != nil {
		return OrderPrintProgress{}, fmt.Errorf("get order print progress for %s: %w", orderID, err)
	}
	return out, nil
}
