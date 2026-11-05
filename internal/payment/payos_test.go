package payment_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bangbc-luna/lunain-travel/internal/payment"
)

const testChecksum = "test-checksum-key"

func hmacHex(key, data string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

func newClient(t *testing.T, h http.HandlerFunc) *payment.PayOS {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return payment.NewPayOS(payment.PayOSConfig{
		ClientID: "cid", APIKey: "akey", ChecksumKey: testChecksum, BaseURL: srv.URL,
		ReturnURL: "https://app/return", CancelURL: "https://app/cancel",
	}, srv.Client())
}

func TestCreateLink_SignsAndSendsHeaders(t *testing.T) {
	var called bool
	p := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.URL.Path != "/v2/payment-requests" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-client-id") != "cid" || r.Header.Get("x-api-key") != "akey" {
			t.Errorf("bad auth headers: %v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		desc := body["description"].(string)
		if desc != "LABCD2345" {
			t.Errorf("description = %q", desc)
		}
		want := hmacHex(testChecksum, "amount=150000&cancelUrl=https://app/cancel&description="+desc+
			"&orderCode=42&returnUrl=https://app/return")
		if body["signature"] != want {
			t.Errorf("signature = %v, want %s", body["signature"], want)
		}
		if body["orderCode"].(float64) != 42 || body["amount"].(float64) != 150000 {
			t.Errorf("body = %v", body)
		}
		if body["expiredAt"].(float64) != 1900000000 {
			t.Errorf("expiredAt = %v", body["expiredAt"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "desc": "success", "data": map[string]any{
			"checkoutUrl": "https://pay.payos.vn/web/abc", "qrCode": "QR", "paymentLinkId": "link1",
		}})
	})
	link, err := p.CreateLink(context.Background(), payment.LinkRequest{
		OrderCode: 42, Amount: 150000,
		Description: "LABCD2345",
		ExpiresAt:   time.Unix(1900000000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("server not called")
	}
	if link.CheckoutURL != "https://pay.payos.vn/web/abc" || link.QRCode != "QR" || link.ProviderLinkID != "link1" {
		t.Errorf("link = %+v", link)
	}
}

func TestCreateLink_ErrorCode(t *testing.T) {
	p := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"01","desc":"Invalid params"}`))
	})
	_, err := p.CreateLink(context.Background(), payment.LinkRequest{OrderCode: 1, Amount: 1000, Description: "LABCD2345"})
	if err == nil || !strings.Contains(err.Error(), "Invalid params") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateLink_ProviderError(t *testing.T) {
	p := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"01","desc":"Invalid params"}`))
	})
	_, err := p.CreateLink(context.Background(), payment.LinkRequest{OrderCode: 1, Amount: 1000, Description: "LABCD2345"})
	var pe *payment.ProviderError
	if !errors.As(err, &pe) || pe.Code != "01" || pe.Desc != "Invalid params" {
		t.Fatalf("err = %v, want *ProviderError{01}", err)
	}
}

func TestCreateLink_ValidatesDescriptionBeforeHTTP(t *testing.T) {
	var hits int
	p := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"code":"00","desc":"ok","data":{"checkoutUrl":"u","paymentLinkId":"l"}}`))
	})
	ctx := context.Background()
	for _, tc := range []struct {
		desc string
		want error
	}{
		{"ABCDEFGHIJ", payment.ErrDescriptionTooLong},
		{"Lđ1234567", payment.ErrInvalidDescription},
		{"has space", payment.ErrInvalidDescription},
		{"", payment.ErrInvalidDescription},
	} {
		_, err := p.CreateLink(ctx, payment.LinkRequest{OrderCode: 1, Amount: 1000, Description: tc.desc})
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: err = %v, want %v", tc.desc, err, tc.want)
		}
	}
	if hits != 0 {
		t.Fatalf("server got %d requests for invalid descriptions", hits)
	}
	if _, err := p.CreateLink(ctx, payment.LinkRequest{OrderCode: 1, Amount: 1000, Description: "Abc123XYZ"}); err != nil {
		t.Fatalf("9-char ASCII rejected: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d", hits)
	}
}

func TestDescriptionForCheckout(t *testing.T) {
	got, err := payment.DescriptionForCheckout("COABCD2345")
	if err != nil || got != "LABCD2345" || len(got) != payment.MaxDescriptionLen {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "COABCD234", "COABCD23456", "XXABCD2345", "COABCD234I", "COabcd2345", "COABCD234U"} {
		if _, err := payment.DescriptionForCheckout(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGetStatus_ProviderError(t *testing.T) {
	p := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"01","desc":"not found"}`))
	})
	_, err := p.GetStatus(context.Background(), 42)
	var pe *payment.ProviderError
	if !errors.As(err, &pe) || pe.Code != "01" {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateLink_HTTPError(t *testing.T) {
	p := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := p.CreateLink(context.Background(), payment.LinkRequest{OrderCode: 1, Amount: 1000, Description: "LABCD2345"}); err == nil {
		t.Fatal("want error")
	}
}

func TestGetStatus(t *testing.T) {
	p := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/payment-requests/42" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-client-id") != "cid" || r.Header.Get("x-api-key") != "akey" {
			t.Errorf("bad auth headers")
		}
		_, _ = w.Write([]byte(`{"code":"00","desc":"success","data":{"orderCode":42,"status":"PAID","amountPaid":150000}}`))
	})
	st, err := p.GetStatus(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "PAID" || st.AmountPaid != 150000 {
		t.Errorf("status = %+v", st)
	}
}

func TestCancelLink(t *testing.T) {
	var called bool
	p := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.URL.Path != "/v2/payment-requests/42/cancel" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":"00","desc":"success","data":{"status":"CANCELLED"}}`))
	})
	if err := p.CancelLink(context.Background(), 42); err != nil || !called {
		t.Fatalf("err=%v called=%v", err, called)
	}

	p2 := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"101","desc":"cannot cancel"}`))
	})
	if err := p2.CancelLink(context.Background(), 42); err == nil {
		t.Fatal("want error on code != 00")
	}
}

// Vector published at
// https://payos.vn/docs/tich-hop-webhook/kiem-tra-du-lieu-voi-signature/
// (the signature in the webhook JSON example on the webhook page does not
// match; the one below is the one the code samples reproduce).
const (
	docChecksum  = "1a54716c8f0efb2744fb28b6e38b25da7f67a925d98bc1c18bd8faaecadd7675"
	docSignature = "412e915d2871504ed31be63c8f62a149a4410d34c4c42affc9006ef9917eaa03"
	docWebhook   = `{"code":"00","desc":"success","success":true,"data":{"orderCode":123,"amount":3000,` +
		`"description":"VQRIO123","accountNumber":"12345678","reference":"TF230204212323",` +
		`"transactionDateTime":"2023-02-04 18:25:00","currency":"VND",` +
		`"paymentLinkId":"124c33293c43417ab7879e14c8d9eb18","code":"00","desc":"Thành công",` +
		`"counterAccountBankId":"","counterAccountBankName":"","counterAccountName":"",` +
		`"counterAccountNumber":"","virtualAccountName":"","virtualAccountNumber":""},` +
		`"signature":"` + docSignature + `"}`
)

func TestVerifyWebhook_ValidAndTampered(t *testing.T) {
	p := payment.NewPayOS(payment.PayOSConfig{ChecksumKey: docChecksum}, nil)

	d, err := p.VerifyWebhook([]byte(docWebhook))
	if err != nil {
		t.Fatalf("doc vector rejected: %v", err)
	}
	if d.OrderCode != 123 || d.Amount != 3000 || d.Reference != "TF230204212323" {
		t.Errorf("data = %+v", d)
	}
	if !d.TransactionAt.Equal(time.Date(2023, 2, 4, 18, 25, 0, 0, time.FixedZone("ICT", 7*3600))) {
		t.Errorf("TransactionAt = %v", d.TransactionAt)
	}
	if !json.Valid(d.Raw) {
		t.Errorf("Raw not json: %s", d.Raw)
	}

	tampered := strings.Replace(docWebhook, `"amount":3000`, `"amount":9000`, 1)
	if _, err := p.VerifyWebhook([]byte(tampered)); !errors.Is(err, payment.ErrInvalidSignature) {
		t.Errorf("tampered: err = %v, want ErrInvalidSignature", err)
	}
	wrongKey := payment.NewPayOS(payment.PayOSConfig{ChecksumKey: "other"}, nil)
	if _, err := wrongKey.VerifyWebhook([]byte(docWebhook)); !errors.Is(err, payment.ErrInvalidSignature) {
		t.Errorf("wrong key: err = %v", err)
	}
	if _, err := p.VerifyWebhook([]byte(`not json`)); err == nil || errors.Is(err, payment.ErrInvalidSignature) {
		t.Errorf("malformed: err = %v", err)
	}
	noSig := strings.Replace(docWebhook, docSignature, "", 1)
	if _, err := p.VerifyWebhook([]byte(noSig)); !errors.Is(err, payment.ErrInvalidSignature) {
		t.Errorf("missing signature: err = %v", err)
	}
}

func TestFake_MarkPaidProducesVerifiableWebhook(t *testing.T) {
	f := payment.NewFake()
	ctx := context.Background()
	link, err := f.CreateLink(ctx, payment.LinkRequest{OrderCode: 7, Amount: 5000, Description: "LABCD2345"})
	if err != nil {
		t.Fatal(err)
	}
	if link.CheckoutURL != "http://localhost/fake-pay/7" {
		t.Errorf("url = %q", link.CheckoutURL)
	}
	if st, _ := f.GetStatus(ctx, 7); st.State != "PENDING" {
		t.Errorf("state = %q", st.State)
	}
	body := f.MarkPaid(7, 5000)
	d, err := f.VerifyWebhook(body)
	if err != nil {
		t.Fatal(err)
	}
	if d.OrderCode != 7 || d.Amount != 5000 || d.Reference == "" || d.TransactionAt.IsZero() {
		t.Errorf("data = %+v", d)
	}
	if st, _ := f.GetStatus(ctx, 7); st.State != "PAID" || st.AmountPaid != 5000 {
		t.Errorf("status = %+v", st)
	}
	// A PayOS client using the fixed fake key accepts it too; tampering fails.
	p := payment.NewPayOS(payment.PayOSConfig{ChecksumKey: payment.FakeChecksumKey}, nil)
	if _, err := p.VerifyWebhook(body); err != nil {
		t.Errorf("payos verify: %v", err)
	}
	bad := strings.Replace(string(body), `"amount":5000`, `"amount":1`, 1)
	if _, err := f.VerifyWebhook([]byte(bad)); !errors.Is(err, payment.ErrInvalidSignature) {
		t.Errorf("tampered: %v", err)
	}
	if err := f.CancelLink(ctx, 7); err == nil {
		t.Error("cancel of paid link should fail")
	}
}
