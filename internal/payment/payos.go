// Package payment: PayOS client, in-memory fake provider and (in later files)
// payment orchestration and webhook handling.
package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultPayOSBaseURL is the production PayOS merchant API.
const DefaultPayOSBaseURL = "https://api-merchant.payos.vn"

const (
	requestTimeout = 10 * time.Second
	successCode    = "00"
)

// MaxDescriptionLen is the longest payment-link description we send. PayOS
// docs (https://payos.vn/docs/api/) allow 9 characters for bank accounts not
// linked through PayOS and 25 for linked ones; we use the safe 9 for both.
const MaxDescriptionLen = 9

var (
	// ErrInvalidDescription: description is empty or not ASCII [A-Za-z0-9].
	ErrInvalidDescription = errors.New("payment: description must be ASCII letters/digits only")
	// ErrDescriptionTooLong: description exceeds MaxDescriptionLen.
	ErrDescriptionTooLong = errors.New("payment: description too long")
)

// ProviderError is a well-formed PayOS answer with code != "00", as opposed
// to a transport or decoding failure.
type ProviderError struct {
	Code string
	Desc string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("payos: code %s: %s", e.Code, e.Desc)
}

func validateDescription(d string) error {
	if d == "" {
		return ErrInvalidDescription
	}
	for i := 0; i < len(d); i++ {
		c := d[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return ErrInvalidDescription
		}
	}
	if len(d) > MaxDescriptionLen {
		return ErrDescriptionTooLong
	}
	return nil
}

var checkoutCodeRe = regexp.MustCompile(`^CO[0-9A-HJKMNP-TV-Z]{8}$`)

// DescriptionForCheckout derives the 9-character transfer description
// ("L" + the 8 random characters) from a checkout code "CO" + 8 chars.
func DescriptionForCheckout(checkoutCode string) (string, error) {
	if !checkoutCodeRe.MatchString(checkoutCode) {
		return "", fmt.Errorf("payment: invalid checkout code %q", checkoutCode)
	}
	return "L" + checkoutCode[2:], nil
}

// ErrInvalidSignature is returned when a webhook signature does not match.
var ErrInvalidSignature = errors.New("payment: invalid webhook signature")

// Payment-link states reported by PayOS (Status.State). The full set, from
// the official payos-lib-node PaymentLinkStatus type:
// PENDING | CANCELLED | UNDERPAID | PAID | EXPIRED | PROCESSING | FAILED.
// Any other value is passed through as is; callers decide by AmountPaid.
const (
	StatePending    = "PENDING"
	StatePaid       = "PAID"
	StateUnderpaid  = "UNDERPAID"
	StateProcessing = "PROCESSING"
	StateCancelled  = "CANCELLED"
	StateExpired    = "EXPIRED"
	StateFailed     = "FAILED"
)

// orderNotFoundDesc is the PayOS error text for an unknown payment link
// ("Mã thanh toán không tồn tại", listed in the API reference error list).
// The docs give no numeric code for it; "101" is the code PayOS integrations
// commonly report and the one the Fake uses.
const (
	orderNotFoundDesc = "Mã thanh toán không tồn tại"
	orderNotFoundCode = "101"
)

// IsOrderNotFound reports whether err is the provider saying the order code
// has no payment link.
func IsOrderNotFound(err error) bool {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Code == orderNotFoundCode || strings.Contains(pe.Desc, orderNotFoundDesc)
}

// LinkRequest describes a payment link to create. Return and cancel URLs
// come from configuration.
type LinkRequest struct {
	OrderCode   int64
	Amount      int64
	Description string
	ExpiresAt   time.Time
}

// Link is a created payment link.
type Link struct {
	CheckoutURL    string
	QRCode         string
	ProviderLinkID string
}

// Status is the provider-side state of a payment link.
type Status struct {
	State      string
	AmountPaid int64
	// Transactions are the transfers the provider lists on the link,
	// informational only: the PayOS docs show "transactions" without
	// documenting its fields, so nothing ties a Reference here to the
	// webhook's data.reference (see ReconcilePayments).
	Transactions []StatusTransaction
}

// StatusTransaction is one transfer listed by GetStatus. At is zero when the
// provider sent no parseable time.
type StatusTransaction struct {
	Reference string    `json:"reference"`
	Amount    int64     `json:"amount"`
	At        time.Time `json:"at"`
}

// WebhookData is the verified content of a payment webhook.
type WebhookData struct {
	OrderCode int64
	Amount    int64
	Reference string
	// TransactionAt is zero if the provider sent an unparseable timestamp.
	TransactionAt time.Time
	// TransactionDateTime is the raw timestamp string.
	TransactionDateTime string
	// PaymentLinkID is the provider's id of the payment link.
	PaymentLinkID string
	// Raw is the original "data" object.
	Raw json.RawMessage
	// Canonical is the signed canonical string of Raw
	// (CanonicalDataString): identical for redeliveries of one transfer
	// whatever their JSON formatting.
	Canonical string
}

// Provider is a payment gateway.
type Provider interface {
	CreateLink(ctx context.Context, req LinkRequest) (Link, error)
	GetStatus(ctx context.Context, orderCode int64) (Status, error)
	CancelLink(ctx context.Context, orderCode int64) error
	VerifyWebhook(body []byte) (WebhookData, error)
}

// PayOSConfig configures the PayOS client.
type PayOSConfig struct {
	ClientID    string
	APIKey      string
	ChecksumKey string
	BaseURL     string
	ReturnURL   string
	CancelURL   string
}

// PayOS is the REST client for payOS (no SDK).
type PayOS struct {
	cfg PayOSConfig
	hc  *http.Client
}

var _ Provider = (*PayOS)(nil)

// NewPayOS builds a client. hc may be nil.
func NewPayOS(cfg PayOSConfig, hc *http.Client) *PayOS {
	if hc == nil {
		hc = &http.Client{}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultPayOSBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &PayOS{cfg: cfg, hc: hc}
}

type envelope struct {
	Code string          `json:"code"`
	Desc string          `json:"desc"`
	Data json.RawMessage `json:"data"`
}

func (p *PayOS) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("payos: marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.BaseURL+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("payos: build request: %w", err)
	}
	req.Header.Set("x-client-id", p.cfg.ClientID)
	req.Header.Set("x-api-key", p.cfg.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("payos: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("payos: read response: %w", err)
	}
	var env envelope
	jsonErr := json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if jsonErr == nil && env.Desc != "" {
			return nil, fmt.Errorf("payos: %s %s: http %d: code %s: %s", method, path, resp.StatusCode, env.Code, env.Desc)
		}
		return nil, fmt.Errorf("payos: %s %s: http %d", method, path, resp.StatusCode)
	}
	if jsonErr != nil {
		return nil, fmt.Errorf("payos: decode response: %w", jsonErr)
	}
	if env.Code != successCode {
		return nil, fmt.Errorf("payos: %s %s: %w", method, path, &ProviderError{Code: env.Code, Desc: env.Desc})
	}
	return env.Data, nil
}

// CreateLink creates a payment link (POST /v2/payment-requests).
func (p *PayOS) CreateLink(ctx context.Context, req LinkRequest) (Link, error) {
	desc := req.Description
	if err := validateDescription(desc); err != nil {
		return Link{}, err
	}
	sigData := "amount=" + strconv.FormatInt(req.Amount, 10) +
		"&cancelUrl=" + p.cfg.CancelURL +
		"&description=" + desc +
		"&orderCode=" + strconv.FormatInt(req.OrderCode, 10) +
		"&returnUrl=" + p.cfg.ReturnURL
	body := map[string]any{
		"orderCode":   req.OrderCode,
		"amount":      req.Amount,
		"description": desc,
		"cancelUrl":   p.cfg.CancelURL,
		"returnUrl":   p.cfg.ReturnURL,
		"signature":   hmacSHA256Hex(p.cfg.ChecksumKey, sigData),
	}
	if !req.ExpiresAt.IsZero() {
		body["expiredAt"] = req.ExpiresAt.Unix()
	}
	data, err := p.do(ctx, http.MethodPost, "/v2/payment-requests", body)
	if err != nil {
		return Link{}, err
	}
	var d struct {
		CheckoutURL   string `json:"checkoutUrl"`
		QRCode        string `json:"qrCode"`
		PaymentLinkID string `json:"paymentLinkId"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return Link{}, fmt.Errorf("payos: decode create data: %w", err)
	}
	if d.CheckoutURL == "" {
		return Link{}, errors.New("payos: create link: empty checkoutUrl")
	}
	return Link{CheckoutURL: d.CheckoutURL, QRCode: d.QRCode, ProviderLinkID: d.PaymentLinkID}, nil
}

// GetStatus looks a link up by order code (GET /v2/payment-requests/{id}).
func (p *PayOS) GetStatus(ctx context.Context, orderCode int64) (Status, error) {
	data, err := p.do(ctx, http.MethodGet, "/v2/payment-requests/"+strconv.FormatInt(orderCode, 10), nil)
	if err != nil {
		return Status{}, err
	}
	var d struct {
		Status       string          `json:"status"`
		AmountPaid   int64           `json:"amountPaid"`
		Transactions json.RawMessage `json:"transactions"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return Status{}, fmt.Errorf("payos: decode status data: %w", err)
	}
	return Status{State: d.Status, AmountPaid: d.AmountPaid, Transactions: decodeStatusTransactions(d.Transactions)}, nil
}

// decodeStatusTransactions reads the "transactions" of a payment link. The
// docs (https://payos.vn/docs/api/, "Lấy thông tin link thanh toán") show it
// as an empty object and document no field, so it is read leniently with the
// field names of the webhook data (reference, amount, transactionDateTime):
// anything but an array of objects gives no transactions, never an error
// (the status and amount paid must not be lost to it).
func decodeStatusTransactions(raw json.RawMessage) []StatusTransaction {
	var items []struct {
		Reference           string `json:"reference"`
		Amount              int64  `json:"amount"`
		TransactionDateTime string `json:"transactionDateTime"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return nil
	}
	out := make([]StatusTransaction, len(items))
	for i, it := range items {
		out[i] = StatusTransaction{Reference: it.Reference, Amount: it.Amount}
		if t, err := time.ParseInLocation(txTimeLayout, it.TransactionDateTime, vnZone); err == nil {
			out[i].At = t
		}
	}
	return out
}

// CancelLink cancels a link (POST /v2/payment-requests/{id}/cancel).
func (p *PayOS) CancelLink(ctx context.Context, orderCode int64) error {
	_, err := p.do(ctx, http.MethodPost, "/v2/payment-requests/"+strconv.FormatInt(orderCode, 10)+"/cancel", map[string]any{})
	return err
}

// VerifyWebhook checks the signature of a webhook body and returns its data.
func (p *PayOS) VerifyWebhook(body []byte) (WebhookData, error) {
	return verifyWebhook(p.cfg.ChecksumKey, body)
}

func hmacSHA256Hex(key, data string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

// signJSONObject signs a JSON object the way PayOS signs webhook data:
// keys sorted alphabetically, "k=v" joined by "&"; null/undefined (and the
// strings "null"/"undefined") become ""; arrays and nested objects are
// serialised as compact JSON with object keys sorted; numbers and booleans
// are written as-is.
func signJSONObject(key string, raw []byte) (string, error) {
	canon, err := CanonicalDataString(raw)
	if err != nil {
		return "", err
	}
	return hmacSHA256Hex(key, canon), nil
}

// CanonicalDataString is the exact string PayOS signs for a webhook "data"
// object (see signJSONObject): sorted "k=v" pairs joined by "&". It does not
// depend on the JSON's whitespace, key order or number spelling.
func CanonicalDataString(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return "", fmt.Errorf("payos: decode data: %w", err)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, err := signValue(obj[k])
		if err != nil {
			return "", err
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "&"), nil
}

func signValue(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		if x == "null" || x == "undefined" {
			return "", nil
		}
		return x, nil
	case json.Number:
		return x.String(), nil
	case bool:
		return strconv.FormatBool(x), nil
	default: // []any, map[string]any: encoding/json sorts map keys
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(x); err != nil {
			return "", fmt.Errorf("payos: encode nested value: %w", err)
		}
		return strings.TrimSuffix(buf.String(), "\n"), nil
	}
}

func verifyWebhook(checksumKey string, body []byte) (WebhookData, error) {
	var w struct {
		Code      string          `json:"code"`
		Desc      string          `json:"desc"`
		Data      json.RawMessage `json:"data"`
		Signature string          `json:"signature"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return WebhookData{}, fmt.Errorf("payos: decode webhook: %w", err)
	}
	if len(w.Data) == 0 || string(w.Data) == "null" {
		return WebhookData{}, errors.New("payos: webhook has no data")
	}
	canon, err := CanonicalDataString(w.Data)
	if err != nil {
		return WebhookData{}, err
	}
	want := hmacSHA256Hex(checksumKey, canon)
	got, err := hex.DecodeString(strings.ToLower(w.Signature))
	wantRaw, _ := hex.DecodeString(want)
	if err != nil || !hmac.Equal(got, wantRaw) {
		return WebhookData{}, ErrInvalidSignature
	}
	if w.Code != successCode {
		return WebhookData{}, fmt.Errorf("payos: webhook code %s: %s", w.Code, w.Desc)
	}
	var d struct {
		OrderCode           int64  `json:"orderCode"`
		Amount              int64  `json:"amount"`
		Reference           string `json:"reference"`
		TransactionDateTime string `json:"transactionDateTime"`
		PaymentLinkID       string `json:"paymentLinkId"`
	}
	if err := json.Unmarshal(w.Data, &d); err != nil {
		return WebhookData{}, fmt.Errorf("payos: decode webhook data: %w", err)
	}
	out := WebhookData{
		OrderCode: d.OrderCode, Amount: d.Amount, Reference: d.Reference, Raw: w.Data, Canonical: canon,
		TransactionDateTime: d.TransactionDateTime, PaymentLinkID: d.PaymentLinkID,
	}
	if t, err := time.ParseInLocation(txTimeLayout, d.TransactionDateTime, vnZone); err == nil {
		out.TransactionAt = t
	}
	return out, nil
}

const txTimeLayout = "2006-01-02 15:04:05"

// vnZone is UTC+7 (PayOS timestamps are Vietnam local time); a fixed zone
// avoids depending on tzdata.
var vnZone = time.FixedZone("ICT", 7*3600)
