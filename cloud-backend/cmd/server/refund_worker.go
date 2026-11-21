package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"xeroxgo/cloud-backend/internal/payments"
	"xeroxgo/cloud-backend/internal/store"
)

// A durable worker retries each refund with the SAME provider idempotency key.
// A Pi safe-stop attestation is mandatory; PAUSED_ERROR alone is not sufficient.
func (s *appServer) startRefundWorker(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.processRefundsOnce(ctx); err != nil {
			log.Printf("[REFUND] reconciliation error: %v", err)
		}
		select {
		case <-ctx.Done(): return
		case <-ticker.C:
		}
	}
}

func (s *appServer) processRefundsOnce(ctx context.Context) error {
	if s.db == nil { return nil }
	provider := s.getPaymentProvider()
	refunder, ok := provider.(payments.RefundProvider)
	if !ok { return fmt.Errorf("payment provider %s does not support refunds", provider.Name()) }

	retryCutoff := time.Now().UTC().Add(-60 * time.Second)
	pending, err := s.db.ListPendingRefunds(ctx, retryCutoff)
	if err != nil { return err }
	for _, item := range pending {
		if item.Provider != provider.Name() && !(provider.Name() == "mock" && item.Provider == "mock_demo") {
			log.Printf("[REFUND] provider mismatch for %s: record=%s active=%s", item.OrderID, item.Provider, provider.Name())
			continue
		}
		subCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		result, err := refunder.GetRefund(subCtx, item.ProviderRefundID)
		cancel()
		if err != nil {
			log.Printf("[REFUND] refund %s remains pending: %v", item.ProviderRefundID, err)
			continue
		}
		switch result.Status {
		case "processed":
			_ = s.db.RecordRefundResult(ctx, item.OrderID, result.ID, store.RefundProcessed, "")
		case "failed":
			_ = s.db.RecordRefundResult(ctx, item.OrderID, result.ID, store.RefundFailed, "provider reported failed")
		}
	}

	ready, err := s.db.ListActionableRefunds(ctx, retryCutoff)
	if err != nil { return err }
	for _, item := range ready {
		if item.Provider != provider.Name() && !(provider.Name() == "mock" && item.Provider == "mock_demo") {
			log.Printf("[REFUND] provider mismatch for %s: record=%s active=%s", item.OrderID, item.Provider, provider.Name())
			continue
		}
		order, err := s.db.GetOrder(ctx, item.OrderID)
		if err != nil || order == nil { continue }
		if order.Status == "COMPLETED" {
			log.Printf("[REFUND] completion/refund conflict for %s: operator review required", item.OrderID)
			continue
		}
		claimed, err := s.db.ClaimRefund(ctx, item.OrderID, retryCutoff)
		if err != nil || !claimed { continue }
		subCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		result, err := refunder.Refund(subCtx, item.PaymentID, item.AmountPaise, item.IdempotencyKey)
		cancel()
		if err != nil {
			_ = s.db.RecordRefundResult(ctx, item.OrderID, item.ProviderRefundID, store.RefundFailed, truncateRefundError(err.Error()))
			continue
		}
		switch result.Status {
		case "processed":
			err = s.db.RecordRefundResult(ctx, item.OrderID, result.ID, store.RefundProcessed, "")
		case "pending":
			err = s.db.RecordRefundResult(ctx, item.OrderID, result.ID, store.RefundPending, "")
		default:
			err = s.db.RecordRefundResult(ctx, item.OrderID, result.ID, store.RefundFailed, "provider refund failed")
		}
		if err != nil { log.Printf("[REFUND] persistence error for %s: %v", item.OrderID, err) }
	}
	return nil
}
func truncateRefundError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 220 { s = s[:220] }
	return s
}
