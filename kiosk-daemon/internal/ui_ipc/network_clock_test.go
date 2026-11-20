package ui_ipc

import (
	"testing"
	"time"
)

func TestNetworkClock_OffsetAndNow(t *testing.T) {
	// 1. Initially default offset
	now := NetworkNow()
	if now.IsZero() {
		t.Fatalf("expected non-zero time")
	}

	// 2. Set custom network anchor (e.g. 2026-04-28 11:24:00)
	target := time.Date(2026, time.April, 28, 11, 24, 0, 0, time.UTC)
	SetNetworkTime(target)

	got := NetworkNow()
	diff := got.Sub(target)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("expected NetworkNow to be close to %v, got %v (diff: %v)", target, got, diff)
	}
}
