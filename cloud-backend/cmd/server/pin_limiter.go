package main

import (
	"context"
	"sync"
	"time"

	"xeroxgo/cloud-backend/internal/store"
)

type pinLimiterEntry struct {
	failures  int
	firstFail time.Time
	lastFail  time.Time
	blockedTo time.Time
}

// PINRateLimiter enforces rate limiting on PIN lookup attempts.
// When a db store is supplied, rate limits are persisted durably in the database,
// surviving server restarts and sharing state across multi-instance cloud deployments.
// When db is nil, it falls back to thread-safe in-memory tracking.
type PINRateLimiter struct {
	db            *store.Store
	mu            sync.Mutex
	maxFailures   int
	window        time.Duration
	blockDuration time.Duration
	records       map[string]*pinLimiterEntry
}

// NewPINRateLimiter creates a rate limiter for PIN lookup attempts.
func NewPINRateLimiter(db *store.Store, maxFailures int, window, blockDuration time.Duration) *PINRateLimiter {
	return &PINRateLimiter{
		db:            db,
		maxFailures:   maxFailures,
		window:        window,
		blockDuration: blockDuration,
		records:       make(map[string]*pinLimiterEntry),
	}
}

// Check inspects whether kioskID is currently blocked.
// Returns (allowed, retryAfter).
func (l *PINRateLimiter) Check(ctx context.Context, kioskID string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	if l.db != nil {
		allowed, retryAfter, err := l.db.CheckRateLimit(ctx, kioskID, now)
		if err == nil {
			return allowed, retryAfter
		}
		// Fall through to in-memory check on DB error
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.records[kioskID]
	if !ok {
		return true, 0
	}

	if now.Before(entry.blockedTo) {
		return false, entry.blockedTo.Sub(now)
	}

	if !entry.blockedTo.IsZero() && !now.Before(entry.blockedTo) {
		delete(l.records, kioskID)
		return true, 0
	}

	if !entry.firstFail.IsZero() && now.Sub(entry.firstFail) > l.window {
		delete(l.records, kioskID)
		return true, 0
	}

	return true, 0
}

// RecordFailure records a failed PIN lookup attempt.
func (l *PINRateLimiter) RecordFailure(ctx context.Context, kioskID string, now time.Time) (bool, time.Duration) {
	if l == nil {
		return false, 0
	}
	if l.db != nil {
		blocked, retryAfter, err := l.db.RecordRateLimitFailure(ctx, kioskID, now, l.maxFailures, l.window, l.blockDuration)
		if err == nil {
			return blocked, retryAfter
		}
		// Fall through to in-memory on DB error
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.records) > 2048 {
		for k, e := range l.records {
			if !e.blockedTo.IsZero() && !now.Before(e.blockedTo) {
				delete(l.records, k)
			} else if now.Sub(e.lastFail) > l.window+l.blockDuration {
				delete(l.records, k)
			}
		}
	}

	entry, ok := l.records[kioskID]
	if !ok {
		entry = &pinLimiterEntry{
			failures:  1,
			firstFail: now,
			lastFail:  now,
		}
		l.records[kioskID] = entry
		if entry.failures >= l.maxFailures {
			entry.blockedTo = now.Add(l.blockDuration)
			return true, l.blockDuration
		}
		return false, 0
	}

	if now.Before(entry.blockedTo) {
		return true, entry.blockedTo.Sub(now)
	}

	if now.Sub(entry.firstFail) > l.window {
		entry.failures = 1
		entry.firstFail = now
		entry.lastFail = now
		entry.blockedTo = time.Time{}
		return false, 0
	}

	entry.failures++
	entry.lastFail = now

	if entry.failures >= l.maxFailures {
		entry.blockedTo = now.Add(l.blockDuration)
		return true, l.blockDuration
	}

	return false, 0
}

// RecordSuccess clears failure tracking for the specified kiosk.
func (l *PINRateLimiter) RecordSuccess(ctx context.Context, kioskID string) {
	if l == nil {
		return
	}
	if l.db != nil {
		_ = l.db.RecordRateLimitSuccess(ctx, kioskID)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.records, kioskID)
}

// Reset clears all in-memory and database rate limit tracking.
func (l *PINRateLimiter) Reset(ctx context.Context) {
	if l == nil {
		return
	}
	if l.db != nil {
		_ = l.db.ResetRateLimits(ctx)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = make(map[string]*pinLimiterEntry)
}
