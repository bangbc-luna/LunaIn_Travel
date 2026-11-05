package payment_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
)

func TestFake_DuplicateOrderCodeAndLostResponse(t *testing.T) {
	f := payment.NewFake()
	ctx := context.Background()
	req := payment.LinkRequest{OrderCode: 9, Amount: 5000, Description: "LABCD2345"}

	f.LoseNextCreateResponse()
	if _, err := f.CreateLink(ctx, req); err == nil {
		t.Fatal("lost response should be an error")
	}
	if st, err := f.GetStatus(ctx, 9); err != nil || st.State != payment.StatePending {
		t.Fatalf("link should exist: %+v %v", st, err)
	}
	var pe *payment.ProviderError
	if _, err := f.CreateLink(ctx, req); !errors.As(err, &pe) {
		t.Fatalf("duplicate order code: err = %v", err)
	}
	if _, err := f.GetStatus(ctx, 10); !errors.As(err, &pe) {
		t.Fatalf("unknown order: err = %v", err)
	}
	req.OrderCode = 10
	if _, err := f.CreateLink(ctx, req); err != nil {
		t.Fatalf("next create: %v", err)
	}
	f.SetState(10, payment.StateExpired)
	if st, _ := f.GetStatus(ctx, 10); st.State != payment.StateExpired {
		t.Fatalf("state = %q", st.State)
	}
}
