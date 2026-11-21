package payments

import (
	"context"
	"testing"
)

func TestMockRefund_ExactlyOnceForSameKey(t *testing.T) {
	p := NewMockProvider("test")
	for i:=0; i<3; i++ {
		got, err := p.Refund(context.Background(), "pay_mock_1", 1500, "xeroxgo_refund_1")
		if err != nil || got.Status != "processed" { t.Fatalf("mock: %+v %v", got, err) }
	}
	if p.RefundedCount != 1 { t.Fatalf("mock duplicate refunds: %d", p.RefundedCount) }
	if _, err := p.Refund(context.Background(), "pay_other", 1500, "xeroxgo_refund_1"); err == nil {
		t.Fatal("reusing refund key for another payment must fail")
	}
}
