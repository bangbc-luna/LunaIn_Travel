package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// FakeChecksumKey is the fixed checksum key used by Fake to sign webhooks.
const FakeChecksumKey = "fake-payos-checksum-key-for-tests-and-dev"

type fakeLink struct {
	req    LinkRequest
	amount int64
	paid   int64
	state  string
}

// Fake is an in-memory Provider for tests and dev.
type Fake struct {
	// Now supplies the transaction time; defaults to time.Now.
	Now func() time.Time

	mu    sync.Mutex
	links map[int64]*fakeLink
	// createErr, when set, makes CreateLink fail (simulates PayOS down).
	createErr error
	// loseResponses > 0: CreateLink creates the link but returns an error,
	// as if its response had been lost on the way back.
	loseResponses int
	// lagStatus > 0: GetStatus reports PENDING with nothing paid (the
	// provider's status lags behind its webhook).
	lagStatus int
	// statusErrs are returned by the next GetStatus calls, in order.
	statusErrs []error
}

// Fake provider error codes (PayOS-like).
const (
	fakeCodeOrderExists   = "231"
	fakeCodeOrderNotFound = orderNotFoundCode
)

var _ Provider = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{Now: time.Now, links: map[int64]*fakeLink{}}
}

// FailCreateLink makes every following CreateLink return err; nil restores
// normal behaviour.
func (f *Fake) FailCreateLink(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErr = err
}

// LoseNextCreateResponse makes the next CreateLink create the link but
// return an error (the response was lost).
func (f *Fake) LoseNextCreateResponse() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loseResponses++
}

// LagNextStatus makes the next GetStatus report PENDING with nothing paid,
// whatever the real state.
func (f *Fake) LagNextStatus() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lagStatus++
}

// FailNextStatus makes the next GetStatus return err.
func (f *Fake) FailNextStatus(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusErrs = append(f.statusErrs, err)
}

// SetState overrides the provider-side state of a known order.
func (f *Fake) SetState(orderCode int64, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.links[orderCode]; ok {
		l.state = state
	}
}

// CreateLink records a pending link. Like PayOS, an order code can only be
// used once: a second CreateLink with it is a *ProviderError.
func (f *Fake) CreateLink(_ context.Context, req LinkRequest) (Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return Link{}, f.createErr
	}
	if err := validateDescription(req.Description); err != nil {
		return Link{}, err
	}
	if _, ok := f.links[req.OrderCode]; ok {
		return Link{}, fmt.Errorf("fake payos: create link: %w",
			&ProviderError{Code: fakeCodeOrderExists, Desc: "Đơn thanh toán đã tồn tại"})
	}
	f.links[req.OrderCode] = &fakeLink{req: req, amount: req.Amount, state: StatePending}
	if f.loseResponses > 0 {
		f.loseResponses--
		return Link{}, errors.New("fake payos: create link: response lost")
	}
	oc := strconv.FormatInt(req.OrderCode, 10)
	return Link{
		CheckoutURL:    "http://localhost/fake-pay/" + oc,
		QRCode:         "fake-qr-" + oc,
		ProviderLinkID: "fake-link-" + oc,
	}, nil
}

// Request returns the LinkRequest of the link created for orderCode.
func (f *Fake) Request(orderCode int64) (LinkRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.links[orderCode]
	if !ok {
		return LinkRequest{}, false
	}
	return l.req, true
}

// GetStatus returns the recorded state; an unknown order is a
// *ProviderError, as PayOS answers it.
func (f *Fake) GetStatus(_ context.Context, orderCode int64) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statusErrs) > 0 {
		err := f.statusErrs[0]
		f.statusErrs = f.statusErrs[1:]
		return Status{}, err
	}
	l, ok := f.links[orderCode]
	if !ok {
		return Status{}, fmt.Errorf("fake payos: order %d: %w", orderCode,
			&ProviderError{Code: fakeCodeOrderNotFound, Desc: orderNotFoundDesc})
	}
	if f.lagStatus > 0 {
		f.lagStatus--
		return Status{State: StatePending}, nil
	}
	return Status{State: l.state, AmountPaid: l.paid}, nil
}

// CancelLink cancels a pending link; paid links cannot be cancelled.
func (f *Fake) CancelLink(_ context.Context, orderCode int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.links[orderCode]
	if !ok {
		return fmt.Errorf("fake payos: order %d not found", orderCode)
	}
	if l.state == StatePaid {
		return fmt.Errorf("fake payos: order %d already paid", orderCode)
	}
	l.state = StateCancelled
	return nil
}

// MarkPaid marks the order paid (creating it if unknown) and returns a
// webhook body signed with FakeChecksumKey, with reference
// "FAKE-REF-<orderCode>" (the same for every call: a repeated delivery).
func (f *Fake) MarkPaid(orderCode, amount int64) []byte {
	return f.MarkPaidRef(orderCode, amount, "FAKE-REF-"+strconv.FormatInt(orderCode, 10))
}

// MarkPaidRef is MarkPaid with the bank reference of the transfer; another
// reference on the same order code is another transfer. Like PayOS,
// amountPaid accumulates over transfers and the link is UNDERPAID while it is
// below the link amount, PAID once it is reached.
func (f *Fake) MarkPaidRef(orderCode, amount int64, reference string) []byte {
	f.mu.Lock()
	l, ok := f.links[orderCode]
	if !ok {
		l = &fakeLink{amount: amount}
		f.links[orderCode] = l
	}
	l.paid += amount
	l.state = StatePaid
	if l.paid < l.amount {
		l.state = StateUnderpaid
	}
	f.mu.Unlock()

	now := f.Now
	if now == nil {
		now = time.Now
	}
	oc := strconv.FormatInt(orderCode, 10)
	data := map[string]any{
		"orderCode":              orderCode,
		"amount":                 amount,
		"description":            "FAKE" + oc,
		"accountNumber":          "12345678",
		"reference":              reference,
		"transactionDateTime":    now().In(vnZone).Format(txTimeLayout),
		"currency":               "VND",
		"paymentLinkId":          "fake-link-" + oc,
		"code":                   "00",
		"desc":                   "Thành công",
		"counterAccountBankId":   "",
		"counterAccountBankName": "",
		"counterAccountName":     "",
		"counterAccountNumber":   "",
		"virtualAccountName":     "",
		"virtualAccountNumber":   "",
	}
	dataJSON, err := json.Marshal(data)
	if err != nil {
		panic(err) // plain scalars; cannot fail
	}
	sig, err := signJSONObject(FakeChecksumKey, dataJSON)
	if err != nil {
		panic(err)
	}
	body, err := json.Marshal(map[string]any{
		"code": "00", "desc": "success", "success": true,
		"data": json.RawMessage(dataJSON), "signature": sig,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// VerifyWebhook verifies a signature exactly as PayOS does, using
// FakeChecksumKey.
func (f *Fake) VerifyWebhook(body []byte) (WebhookData, error) {
	return verifyWebhook(FakeChecksumKey, body)
}
