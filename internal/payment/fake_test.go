package payment_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
)

func TestFake_StatusListsTransfers(t *testing.T) {
	f := payment.NewFake()
	at := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	f.Now = func() time.Time { return at }
	ctx := context.Background()
	if _, err := f.CreateLink(ctx, payment.LinkRequest{OrderCode: 5, Amount: 10000, Description: "LABCD2345"}); err != nil {
		t.Fatal(err)
	}
	f.MarkPaidRef(5, 4000, "REF-A")
	f.MarkPaidRef(5, 6000, "REF-B")
	st, err := f.GetStatus(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != payment.StatePaid || st.AmountPaid != 10000 || len(st.Transactions) != 2 {
		t.Fatalf("status = %+v", st)
	}
	if tx := st.Transactions[0]; tx.Reference != "REF-A" || tx.Amount != 4000 || !tx.At.Equal(at) {
		t.Errorf("first = %+v", tx)
	}
	if tx := st.Transactions[1]; tx.Reference != "REF-B" || tx.Amount != 6000 {
		t.Errorf("second = %+v", tx)
	}
	// A lagging status shows nothing at all.
	f.LagNextStatus()
	if st, _ := f.GetStatus(ctx, 5); st.State != payment.StatePending || st.AmountPaid != 0 || len(st.Transactions) != 0 {
		t.Fatalf("lagging status = %+v", st)
	}
	// The returned slice is a copy.
	st.Transactions[0].Amount = 1
	if again, _ := f.GetStatus(ctx, 5); again.Transactions[0].Amount != 4000 {
		t.Fatal("GetStatus leaked the fake's own slice")
	}
}

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
