package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"xeroxgo/pkg/contracts"
)

func TestStore_PINExpiryAndStorage(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// 1. Order created with zero PINExpiresAt defaults to CreatedAt + 15m
	ord1 := sampleOrder("ord_pin_1", "job_pin_1")
	ord1.Command.DisplayCode = "111111"
	ord1.PINExpiresAt = time.Time{}
	if err := s.CreateOrder(ctx, ord1); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	got1, err := s.GetOrder(ctx, "ord_pin_1")
	if err != nil || got1 == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	expectedExpiry1 := ord1.CreatedAt.Add(15 * time.Minute)
	if !got1.PINExpiresAt.Equal(expectedExpiry1) {
		t.Fatalf("expected default PIN expiry %v, got %v", expectedExpiry1, got1.PINExpiresAt)
	}

	// 2. Order created with explicit PINExpiresAt preserves the exact value
	ord2 := sampleOrder("ord_pin_2", "job_pin_2")
	ord2.Command.DisplayCode = "222222"
	explicitExpiry := ord2.CreatedAt.Add(30 * time.Minute)
	ord2.PINExpiresAt = explicitExpiry
	if err := s.CreateOrder(ctx, ord2); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	got2, err := s.GetOrder(ctx, "ord_pin_2")
	if err != nil || got2 == nil {
		t.Fatalf("GetOrder failed: %v", err)
	}
	if !got2.PINExpiresAt.Equal(explicitExpiry) {
		t.Fatalf("expected explicit PIN expiry %v, got %v", explicitExpiry, got2.PINExpiresAt)
	}
}

func TestStore_AtomicPINReservationAndCollisionRejection(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	baseTime := time.Date(2026, time.November, 10, 12, 0, 0, 0, time.UTC)
	pin := "888999"

	// 1. Create first order with pin
	ord1 := sampleOrder("ord_res_1", "job_res_1")
	ord1.Command.KioskID = "kiosk-01"
	ord1.Command.DisplayCode = pin
	ord1.Status = contracts.JobStatusPendingPayment
	ord1.CreatedAt = baseTime
	ord1.PINExpiresAt = baseTime.Add(15 * time.Minute)

	if err := s.CreateOrder(ctx, ord1); err != nil {
		t.Fatalf("CreateOrder 1 failed: %v", err)
	}

	// 2. Attempt creating second order with same PIN on same kiosk while active -> must fail with ErrPINCollision
	ord2 := sampleOrder("ord_res_2", "job_res_2")
	ord2.Command.KioskID = "kiosk-01"
	ord2.Command.DisplayCode = pin
	ord2.Status = contracts.JobStatusPendingPayment
	ord2.CreatedAt = baseTime.Add(1 * time.Minute)
	ord2.PINExpiresAt = baseTime.Add(16 * time.Minute)

	err := s.CreateOrder(ctx, ord2)
	if err == nil {
		t.Fatalf("expected ErrPINCollision, got nil error")
	}
	if !errors.Is(err, ErrPINCollision) {
		t.Fatalf("expected ErrPINCollision, got: %v", err)
	}

	// 3. Same PIN on different kiosk (kiosk-02) must succeed (kiosk isolation)
	ordOtherKiosk := sampleOrder("ord_res_other", "job_res_other")
	ordOtherKiosk.Command.KioskID = "kiosk-02"
	ordOtherKiosk.Command.DisplayCode = pin
	ordOtherKiosk.Status = contracts.JobStatusPendingPayment
	ordOtherKiosk.CreatedAt = baseTime
	ordOtherKiosk.PINExpiresAt = baseTime.Add(15 * time.Minute)

	if err := s.CreateOrder(ctx, ordOtherKiosk); err != nil {
		t.Fatalf("CreateOrder on different kiosk failed: %v", err)
	}

	// 4. Once ord1 completes, the PIN reservation is freed and can be reused on kiosk-01
	if err := s.UpdateOrderStatus(ctx, "ord_res_1", contracts.JobStatusCompleted); err != nil {
		t.Fatalf("UpdateOrderStatus failed: %v", err)
	}

	ord3 := sampleOrder("ord_res_3", "job_res_3")
	ord3.Command.KioskID = "kiosk-01"
	ord3.Command.DisplayCode = pin
	ord3.Status = contracts.JobStatusPendingPayment
	ord3.CreatedAt = baseTime.Add(2 * time.Minute)
	ord3.PINExpiresAt = baseTime.Add(17 * time.Minute)

	if err := s.CreateOrder(ctx, ord3); err != nil {
		t.Fatalf("CreateOrder after completion should succeed, got: %v", err)
	}
}

func TestStore_PINReuseAfterExpiration(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	baseTime := time.Date(2026, time.November, 10, 10, 0, 0, 0, time.UTC)
	pin := "555444"

	// 1. Create order that expires at baseTime + 5 minutes
	ordOld := sampleOrder("ord_exp_old", "job_exp_old")
	ordOld.Command.KioskID = "kiosk-01"
	ordOld.Command.DisplayCode = pin
	ordOld.Status = contracts.JobStatusPendingPayment
	ordOld.CreatedAt = baseTime
	ordOld.PINExpiresAt = baseTime.Add(5 * time.Minute)

	if err := s.CreateOrder(ctx, ordOld); err != nil {
		t.Fatalf("CreateOrder old failed: %v", err)
	}

	// 2. Advance time past expiration (baseTime + 10m) and create new order with same PIN -> must succeed
	ordNew := sampleOrder("ord_exp_new", "job_exp_new")
	ordNew.Command.KioskID = "kiosk-01"
	ordNew.Command.DisplayCode = pin
	ordNew.Status = contracts.JobStatusPendingPayment
	ordNew.CreatedAt = baseTime.Add(10 * time.Minute)
	ordNew.PINExpiresAt = baseTime.Add(25 * time.Minute)

	if err := s.CreateOrder(ctx, ordNew); err != nil {
		t.Fatalf("CreateOrder after expiration failed: %v", err)
	}
}

func TestStore_ConcurrentOrderCreationUniqueness(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	const numGoroutines = 30
	pin := "777777" // All 30 goroutines contest for the exact same PIN on kiosk-race

	var wg sync.WaitGroup
	successCount := 0
	collisionCount := 0
	var mu sync.Mutex

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			ord := sampleOrder(
				time.Now().Format("ord_race_0405_000000_")+string(rune('A'+idx)),
				time.Now().Format("job_race_0405_000000_")+string(rune('A'+idx)),
			)
			ord.Command.KioskID = "kiosk-race"
			ord.Command.DisplayCode = pin
			ord.Status = contracts.JobStatusPendingPayment
			ord.CreatedAt = time.Now().UTC()
			ord.PINExpiresAt = ord.CreatedAt.Add(15 * time.Minute)

			err := s.CreateOrder(ctx, ord)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successCount++
			} else if errors.Is(err, ErrPINCollision) {
				collisionCount++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}

	wg.Wait()

	// Exactly 1 order must successfully reserve the PIN, and all others must receive ErrPINCollision
	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful reservation, got %d", successCount)
	}
	if collisionCount != numGoroutines-1 {
		t.Fatalf("expected %d collisions, got %d", numGoroutines-1, collisionCount)
	}
}

func TestStore_DurableRateLimiting(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, time.November, 10, 15, 0, 0, 0, time.UTC)
	kioskID := "kiosk-rate"

	// 1. Initially allowed
	allowed, retry, err := s.CheckRateLimit(ctx, kioskID, now)
	if err != nil || !allowed || retry != 0 {
		t.Fatalf("initial CheckRateLimit failed: allowed=%v, retry=%v, err=%v", allowed, retry, err)
	}

	// 2. Record 4 failures within 2-minute window (limit is 5)
	for i := 1; i <= 4; i++ {
		blocked, retryDur, err := s.RecordRateLimitFailure(ctx, kioskID, now.Add(time.Duration(i)*time.Second), 5, 2*time.Minute, 2*time.Minute)
		if err != nil || blocked || retryDur != 0 {
			t.Fatalf("failure %d unexpectedly blocked: %v, %v, %v", i, blocked, retryDur, err)
		}
	}

	// Still allowed before 5th failure
	allowed, _, err = s.CheckRateLimit(ctx, kioskID, now.Add(10*time.Second))
	if err != nil || !allowed {
		t.Fatalf("should still be allowed after 4 failures")
	}

	// 3. 5th failure triggers block for 2 minutes
	blocked, retryDur, err := s.RecordRateLimitFailure(ctx, kioskID, now.Add(15*time.Second), 5, 2*time.Minute, 2*time.Minute)
	if err != nil || !blocked || retryDur != 2*time.Minute {
		t.Fatalf("5th failure failed to block: blocked=%v, retryDur=%v, err=%v", blocked, retryDur, err)
	}

	// CheckRateLimit now returns blocked
	allowed, retry, err = s.CheckRateLimit(ctx, kioskID, now.Add(30*time.Second))
	if err != nil || allowed || retry <= 0 {
		t.Fatalf("expected blocked, got allowed=%v, retry=%v", allowed, retry)
	}

	// 4. Success clears rate limit
	if err := s.RecordRateLimitSuccess(ctx, kioskID); err != nil {
		t.Fatalf("RecordRateLimitSuccess failed: %v", err)
	}
	allowed, _, err = s.CheckRateLimit(ctx, kioskID, now.Add(35*time.Second))
	if err != nil || !allowed {
		t.Fatalf("expected allowed after RecordRateLimitSuccess")
	}
}

func TestStore_IsPINInUse(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	baseTime := time.Date(2026, time.November, 10, 12, 0, 0, 0, time.UTC)
	pin := "123456"

	// Create an active order on kiosk-01
	ord := sampleOrder("ord_pin_active", "job_pin_active")
	ord.Command.KioskID = "kiosk-01"
	ord.Command.DisplayCode = pin
	ord.Status = contracts.JobStatusPendingPayment
	ord.CreatedAt = baseTime
	ord.PINExpiresAt = baseTime.Add(15 * time.Minute)

	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// In use within 15 minutes on kiosk-01
	inUse, err := s.IsPINInUse(ctx, "kiosk-01", pin, baseTime.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("IsPINInUse failed: %v", err)
	}
	if !inUse {
		t.Fatalf("expected PIN %s to be in use on kiosk-01", pin)
	}

	// Kiosk isolation: PIN on kiosk-01 is NOT in use on kiosk-02
	inUseOtherKiosk, err := s.IsPINInUse(ctx, "kiosk-02", pin, baseTime.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("IsPINInUse failed: %v", err)
	}
	if inUseOtherKiosk {
		t.Fatalf("PIN %s should NOT be in use on kiosk-02", pin)
	}

	// Different PIN on same kiosk is NOT in use
	inUseDiffPIN, err := s.IsPINInUse(ctx, "kiosk-01", "654321", baseTime.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("IsPINInUse failed: %v", err)
	}
	if inUseDiffPIN {
		t.Fatalf("PIN 654321 should NOT be in use")
	}

	// After expiry: PIN should no longer be considered in use
	inUseExpired, err := s.IsPINInUse(ctx, "kiosk-01", pin, baseTime.Add(16*time.Minute))
	if err != nil {
		t.Fatalf("IsPINInUse failed: %v", err)
	}
	if inUseExpired {
		t.Fatalf("expired PIN should NOT be considered in use")
	}

	// If status changed to COMPLETED before expiry, PIN is freed
	ordCompleted := sampleOrder("ord_pin_comp", "job_pin_comp")
	ordCompleted.Command.KioskID = "kiosk-01"
	ordCompleted.Command.DisplayCode = "888888"
	ordCompleted.Status = contracts.JobStatusCompleted
	ordCompleted.CreatedAt = baseTime
	ordCompleted.PINExpiresAt = baseTime.Add(15 * time.Minute)
	if err := s.CreateOrder(ctx, ordCompleted); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	inUseCompleted, err := s.IsPINInUse(ctx, "kiosk-01", "888888", baseTime.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("IsPINInUse failed: %v", err)
	}
	if inUseCompleted {
		t.Fatalf("completed order PIN should NOT be considered in use")
	}
}

func TestStore_GetOrderByPIN(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	baseTime := time.Date(2026, time.November, 10, 14, 0, 0, 0, time.UTC)
	pin := "987654"

	ord := sampleOrder("ord_lookup_1", "job_lookup_1")
	ord.Command.KioskID = "kiosk-main"
	ord.Command.DisplayCode = pin
	ord.Status = contracts.JobStatusPendingPayment
	ord.CreatedAt = baseTime
	ord.PINExpiresAt = baseTime.Add(15 * time.Minute)

	if err := s.CreateOrder(ctx, ord); err != nil {
		t.Fatalf("CreateOrder failed: %v", err)
	}

	// 1. Successful lookup by PIN and kioskID
	found, err := s.GetOrderByPIN(ctx, "kiosk-main", pin)
	if err != nil {
		t.Fatalf("GetOrderByPIN failed: %v", err)
	}
	if found == nil {
		t.Fatalf("expected order to be found by PIN")
	}
	if found.OrderID != "ord_lookup_1" {
		t.Fatalf("expected order ID ord_lookup_1, got %s", found.OrderID)
	}
	if found.Command.DisplayCode != pin {
		t.Fatalf("expected PIN %s, got %s", pin, found.Command.DisplayCode)
	}
	if !found.PINExpiresAt.Equal(ord.PINExpiresAt) {
		t.Fatalf("expected PINExpiresAt %v, got %v", ord.PINExpiresAt, found.PINExpiresAt)
	}

	// 2. Kiosk isolation: wrong kiosk returns nil
	wrongKiosk, err := s.GetOrderByPIN(ctx, "kiosk-other", pin)
	if err != nil {
		t.Fatalf("GetOrderByPIN failed: %v", err)
	}
	if wrongKiosk != nil {
		t.Fatalf("expected nil for wrong kiosk, got %+v", wrongKiosk)
	}

	// 3. Unknown PIN returns nil
	unknownPIN, err := s.GetOrderByPIN(ctx, "kiosk-main", "000000")
	if err != nil {
		t.Fatalf("GetOrderByPIN failed: %v", err)
	}
	if unknownPIN != nil {
		t.Fatalf("expected nil for unknown PIN, got %+v", unknownPIN)
	}
}
