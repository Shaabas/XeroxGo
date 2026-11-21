package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"xeroxgo/pkg/contracts"
)

// deliverAuthorizedJobs performs best-effort transport for an order that has
// already committed payment and durable per-document dispatch intents.
// A socket write is not a Pi acknowledgement, and failure is not proof that
// the command was never received. Only the Pi's durable JobID deduplication
// makes replay after reconnect safe.
func (s *appServer) deliverAuthorizedJobs(ctx context.Context, orderID string, commands []contracts.PrintJobCommand) error {
	if len(commands) == 0 {
		return errors.New("no persisted print commands to deliver")
	}
	if s.hub == nil {
		return errors.New("kiosk communication hub unavailable")
	}
	for _, cmd := range commands {
		if err := s.hub.SendJob(ctx, cmd); err != nil {
			s.noteDispatchDeliveryFailure(orderID, cmd.JobID, err)
			return fmt.Errorf("delivery of job %s unresolved: %w", cmd.JobID, err)
		}
		// The intent may already be ACKNOWLEDGED by a fast Pi; a conditional
		// SENT transition must never downgrade that durable acknowledgement.
		if err := s.db.MarkDispatchIntentSent(ctx, cmd.JobID); err != nil {
			s.noteDispatchDeliveryFailure(orderID, cmd.JobID, err)
			return fmt.Errorf("record delivery of job %s: %w", cmd.JobID, err)
		}
	}
	return nil
}

func (s *appServer) noteDispatchDeliveryFailure(orderID, jobID string, deliveryErr error) {
	if s.db == nil || strings.TrimSpace(jobID) == "" || deliveryErr == nil {
		return
	}
	// Do not let a canceled request context erase evidence of a failed send.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.db.RecordDispatchDeliveryFailure(ctx, jobID, deliveryErr.Error()); err != nil {
		log.Printf("[DISPATCH] Could not record delivery error for order=%s job=%s: %v", orderID, jobID, err)
	}
}
