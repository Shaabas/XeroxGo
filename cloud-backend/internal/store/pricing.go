package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/pricing"
)

var (
	ErrSheetTariffNotActive = errors.New("sheet tariffs have not been activated")
	ErrSheetTariffRevisionConflict = errors.New("sheet tariff revision changed; reload before saving")
)

// initSheetTariffSchema initializes the canonical revisioned tariff tables.
// Existing orders and payment amounts are not modified.
func initSheetTariffSchema(ctx context.Context, db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS sheet_tariff_versions (
	revision BIGINT PRIMARY KEY CHECK (revision > 0),
	bw_simplex_paise BIGINT NOT NULL CHECK (bw_simplex_paise BETWEEN 1 AND 1000000),
	bw_duplex_sheet_paise BIGINT NOT NULL CHECK (bw_duplex_sheet_paise BETWEEN 1 AND 1000000),
	color_simplex_paise BIGINT NOT NULL CHECK (color_simplex_paise BETWEEN 1 AND 1000000),
	color_duplex_sheet_paise BIGINT NOT NULL CHECK (color_duplex_sheet_paise BETWEEN 1 AND 1000000),
	color_enabled INTEGER NOT NULL CHECK (color_enabled IN (0,1)),
	changed_by TEXT NOT NULL CHECK (length(changed_by) > 0),
	created_at_ns BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS sheet_tariff_state (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	active_revision BIGINT NOT NULL DEFAULT 0 CHECK (active_revision >= 0)
);`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize sheet tariff schema: %w", err)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO sheet_tariff_state(id, active_revision)
		VALUES (1, 0) ON CONFLICT(id) DO NOTHING;`)
	if err != nil {
		return fmt.Errorf("initialize sheet tariff state: %w", err)
	}
	return nil
}

// GetActiveSheetTariff reads one immutable revision. Unconfigured databases
// intentionally have no active sheet-based prices.
func (s *Store) GetActiveSheetTariff(ctx context.Context) (pricing.Tariff, error) {
	if s == nil || s.db == nil || ctx == nil {
		return pricing.Tariff{}, errors.New("get sheet tariff: invalid store or context")
	}
	var t pricing.Tariff
	var enabled int
	err := s.db.QueryRowContext(ctx, `
SELECT v.revision, v.bw_simplex_paise, v.bw_duplex_sheet_paise,
       v.color_simplex_paise, v.color_duplex_sheet_paise, v.color_enabled
FROM sheet_tariff_state st
JOIN sheet_tariff_versions v ON v.revision = st.active_revision
WHERE st.id = 1;`).Scan(&t.Revision, &t.BWSimplexPaise,
		&t.BWDuplexSheetPaise, &t.ColorSimplexPaise,
		&t.ColorDuplexSheetPaise, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return pricing.Tariff{}, ErrSheetTariffNotActive
	}
	if err != nil {
		return pricing.Tariff{}, fmt.Errorf("get sheet tariff: %w", err)
	}
	if enabled != 0 && enabled != 1 {
		return pricing.Tariff{}, errors.New("get sheet tariff: invalid color flag")
	}
	t.ColorEnabled = enabled == 1
	if err := pricing.ValidateTariff(t); err != nil {
		return pricing.Tariff{}, fmt.Errorf("get sheet tariff: corrupt active tariff: %w", err)
	}
	return t, nil
}

// ActivateSheetTariff atomically publishes a new immutable tariff revision.
// Admin request authentication/authorization must happen before calling it.
// On first activation, expectedRevision=0. Later writes require the revision
// returned from GetActiveSheetTariff, rejecting concurrent/stale admin edits.
func (s *Store) ActivateSheetTariff(ctx context.Context, expectedRevision int64, next pricing.Tariff, changedBy string) (pricing.Tariff, error) {
	if s == nil || s.db == nil || ctx == nil {
		return pricing.Tariff{}, errors.New("activate sheet tariff: invalid store or context")
	}
	if err := ctx.Err(); err != nil {
		return pricing.Tariff{}, err
	}
	changedBy = strings.TrimSpace(changedBy)
	if changedBy == "" || len(changedBy) > 128 {
		return pricing.Tariff{}, errors.New("activate sheet tariff: invalid actor")
	}
	if expectedRevision < 0 || expectedRevision >= int64(1<<63-1) {
		return pricing.Tariff{}, errors.New("activate sheet tariff: invalid expected revision")
	}
	next.Revision = expectedRevision + 1
	if err := pricing.ValidateTariff(next); err != nil {
		return pricing.Tariff{}, fmt.Errorf("activate sheet tariff: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return pricing.Tariff{}, fmt.Errorf("activate sheet tariff: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE sheet_tariff_state
		SET active_revision = ? WHERE id = 1 AND active_revision = ?;`), next.Revision, expectedRevision)
	if err != nil {
		return pricing.Tariff{}, fmt.Errorf("activate sheet tariff: concurrent update failed: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return pricing.Tariff{}, fmt.Errorf("activate sheet tariff: count updated rows: %w", err)
	}
	if count != 1 {
		return pricing.Tariff{}, ErrSheetTariffRevisionConflict
	}

	colorEnabled := 0
	if next.ColorEnabled {
		colorEnabled = 1
	}
	_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO sheet_tariff_versions
		(revision, bw_simplex_paise, bw_duplex_sheet_paise,
		 color_simplex_paise, color_duplex_sheet_paise, color_enabled,
		 changed_by, created_at_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?);`),
		next.Revision, next.BWSimplexPaise, next.BWDuplexSheetPaise,
		next.ColorSimplexPaise, next.ColorDuplexSheetPaise,
		colorEnabled, changedBy, time.Now().UTC().UnixNano())
	if err != nil {
		return pricing.Tariff{}, fmt.Errorf("activate sheet tariff: write revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return pricing.Tariff{}, fmt.Errorf("activate sheet tariff: commit: %w", err)
	}
	return next, nil
}
