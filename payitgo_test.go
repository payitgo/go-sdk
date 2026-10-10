package payitgo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Signed by the gateway's own signer.
const (
	testSecret = "whsec_test_secret"
	testHeader = "t=1760000000,v1=6abd292fdf2ecc51779c161f5d4be2d47236f7a43fbaa9dd7093303904b91a7b"
	testBody   = `{"id":"evt_test_123","type":"payment.confirmed","livemode":false,"created_at":"2026-10-09T12:00:00Z","data":{"id":"pay_123","order_id":"ORD-1","status":"confirmed","usd_amount":"10"}}`
)

func sign(secret string, t int64, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10) + "." + body))
	return "t=" + strconv.FormatInt(t, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestConstructEventAcceptsGatewaySignature(t *testing.T) {
	e, err := ConstructEventWithTolerance([]byte(testBody), testHeader, testSecret, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.Payment()
	if err != nil || e.Type != "payment.confirmed" || e.Livemode || p.OrderID != "ORD-1" || p.USDAmount != "10" {
		t.Fatalf("event %+v payment %+v err %v", e, p, err)
	}
}

func TestConstructEventRejects(t *testing.T) {
	cases := map[string]struct{ body, header, secret string }{
		"changed body": {strings.Replace(testBody, `"10"`, `"1000"`, 1), testHeader, testSecret},
		"wrong secret": {testBody, testHeader, "whsec_other"},
		"no header":    {testBody, "", testSecret},
	}
	for name, c := range cases {
		if _, err := ConstructEventWithTolerance([]byte(c.body), c.header, c.secret, 0); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: want ErrSignature, got %v", name, err)
		}
	}
}

func TestConstructEventTolerance(t *testing.T) {
	if _, err := ConstructEvent([]byte(testBody), testHeader, testSecret); !errors.Is(err, ErrSignature) {
		t.Fatalf("old timestamp accepted: %v", err)
	}
	fresh := sign(testSecret, time.Now().Unix(), testBody)
	if _, err := ConstructEvent([]byte(testBody), fresh, testSecret); err != nil {
		t.Fatalf("fresh delivery refused: %v", err)
	}
}

func TestNewClient(t *testing.T) {
	t.Setenv("PAYITGO_BASE_URL", "")
	if _, err := NewClient("pk_123", WithBaseURL("http://x")); err == nil {
		t.Error("bad key accepted")
	}
	// Without an address the client uses the release's default, or refuses.
	if c, err := NewClient("sk_test_abc"); defaultBaseURL == "" && err == nil {
		t.Error("missing base URL accepted")
	} else if defaultBaseURL != "" && (err != nil || c.baseURL != strings.TrimRight(defaultBaseURL, "/")) {
		t.Errorf("default base URL not used: %v", err)
	}
	c, err := NewClient("sk_live_abc", WithBaseURL("http://x/"))
	if err != nil || !c.Livemode || c.baseURL != "http://x" {
		t.Fatalf("client %+v err %v", c, err)
	}
}

// ── The webhook (IPN), against a fake gateway ───────────────────────────────

const (
	testWebhook  = `{"livemode":false,"url":"https://shop.example/payitgo/ipn","secret":"whsec_abc","created_at":"2026-10-10T09:00:00Z","updated_at":"2026-10-10T09:05:00Z"}`
	testDelivery = `{"id":"whd_1","merchant_id":"mch_1","target":"account","url":"https://shop.example/payitgo/ipn","event_id":"evt_1","event_type":"ping","payload":"{}","status":"pending","attempts":0,"max_attempts":7,"next_attempt_at":"2026-10-10T12:00:00Z","created_at":"2026-10-10T12:00:00Z"}`
)

type recorded struct {
	method, path, query, auth, contentType string
	body                                   map[string]any
}

// fakeGateway answers every request with status and reply, and records it.
func fakeGateway(t *testing.T, status int, reply string, opts ...Option) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*rec = recorded{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type")}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.body); err != nil {
				t.Errorf("request body %q: %v", raw, err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient("sk_test_abc", append([]Option{WithBaseURL(srv.URL), WithMaxRetries(0)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

func TestWebhooksGet(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"webhook":`+testWebhook+`}`)
	w, err := c.Webhooks.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodGet || rec.path != "/v1/webhook" || rec.auth != "Bearer sk_test_abc" || rec.body != nil {
		t.Errorf("request %+v", rec)
	}
	if w.URL != "https://shop.example/payitgo/ipn" || w.Secret != "whsec_abc" || w.Livemode || w.UpdatedAt.IsZero() {
		t.Errorf("webhook %+v", w)
	}
}

func TestWebhooksSet(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"webhook":`+testWebhook+`}`)
	w, err := c.Webhooks.Set(context.Background(), "https://shop.example/payitgo/ipn")
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPut || rec.path != "/v1/webhook" || rec.auth != "Bearer sk_test_abc" || rec.contentType != "application/json" {
		t.Errorf("request %+v", rec)
	}
	if want := map[string]any{"url": "https://shop.example/payitgo/ipn"}; !reflect.DeepEqual(rec.body, want) {
		t.Errorf("body %v, want %v", rec.body, want)
	}
	if w.URL != "https://shop.example/payitgo/ipn" {
		t.Errorf("webhook %+v", w)
	}
}

func TestWebhooksRemove(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"webhook":`+strings.Replace(testWebhook, `"https://shop.example/payitgo/ipn"`, `null`, 1)+`}`)
	w, err := c.Webhooks.Remove(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodDelete || rec.path != "/v1/webhook" || rec.auth != "Bearer sk_test_abc" {
		t.Errorf("request %+v", rec)
	}
	if w.URL != "" || w.Secret != "whsec_abc" {
		t.Errorf("webhook %+v", w)
	}
}

func TestWebhooksTest(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusAccepted, `{"delivery":`+testDelivery+`}`)
	d, err := c.Webhooks.Test(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/webhook/test" || rec.auth != "Bearer sk_test_abc" {
		t.Errorf("request %+v", rec)
	}
	if d.EventType != EventPing || d.Status != DeliveryPending || d.MaxAttempts != 7 || d.Target != TargetAccount {
		t.Errorf("delivery %+v", d)
	}
}

func TestWebhooksErrors(t *testing.T) {
	cases := []struct {
		status  int
		message string
		call    func(*Client) error
	}{
		{http.StatusBadRequest, "no webhook url is set for this mode: set one first", func(c *Client) error {
			_, err := c.Webhooks.Test(context.Background())
			return err
		}},
		{http.StatusForbidden, "a storefront's key cannot change the account's webhook: set webhook_url on its payments instead", func(c *Client) error {
			_, err := c.Webhooks.Set(context.Background(), "https://shop.example/payitgo/ipn")
			return err
		}},
	}
	for _, tc := range cases {
		reply, _ := json.Marshal(map[string]string{"error": tc.message})
		c, _ := fakeGateway(t, tc.status, string(reply))
		var perr *Error
		if err := tc.call(c); !errors.As(err, &perr) || perr.Status != tc.status || perr.Message != tc.message {
			t.Errorf("%d: err %v", tc.status, err)
		}
	}
}

func TestPaymentsCreateWebhookURL(t *testing.T) {
	const reply = `{"id":"pay_1","order_id":"ORD-1","status":"pending","webhook_url":"https://shop.example/own"}`
	cases := []struct {
		name, clientURL, paramURL, want string
	}{
		{"none", "", "", ""},
		{"the call's own", "", "https://shop.example/own", "https://shop.example/own"},
		{"the client's default", "https://shop.example/default", "", "https://shop.example/default"},
		{"the call's own wins", "https://shop.example/default", "https://shop.example/own", "https://shop.example/own"},
	}
	for _, tc := range cases {
		var opts []Option
		if tc.clientURL != "" {
			opts = append(opts, WithWebhookURL(tc.clientURL))
		}
		c, rec := fakeGateway(t, http.StatusCreated, reply, opts...)
		params := &PaymentParams{OrderID: "ORD-1", AmountUSD: "10", WebhookURL: tc.paramURL}
		p, err := c.Payments.Create(context.Background(), params)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := rec.body["webhook_url"].(string); got != tc.want || rec.method != http.MethodPost || rec.path != "/v1/payments" {
			t.Errorf("%s: sent %v to %s %s", tc.name, rec.body, rec.method, rec.path)
		}
		if params.WebhookURL != tc.paramURL {
			t.Errorf("%s: the caller's params changed: %+v", tc.name, params)
		}
		if p.WebhookURL != "https://shop.example/own" {
			t.Errorf("%s: payment %+v", tc.name, p)
		}
	}
}

// testChosen is a payment whose coin the customer chose on the merchant's own page.
const testChosen = `{"id":"pay_1","merchant_id":"mch_1","order_id":"ORD-7","status":"pending","livemode":false,"token":"XRP","chain":"xrp",` +
	`"deposit_address":"rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe","destination_tag":1042,"crypto_amount":"20","received_amount":"0",` +
	`"usd_amount":"10","amount_due_usd":"10","fees_paid_by_customer":false,"network_fee":"0.000012","network_fee_usd":"0.01",` +
	`"settlement":"custodial","received_usd":"0","rate_locked":"0.5","deposits":[],"expires_at":"2026-10-10T12:15:00Z",` +
	`"created_at":"2026-10-10T12:00:00Z","network_fee_note":"XRP Ledger testnet: test XRP only. Include destination tag — required.",` +
	`"qr_payload":"xrpl:rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe?amount=20.000000&dt=1042","checkout_token":"ct_abc",` +
	`"checkout_url":"https://pay.example/ct_abc","description":"Order 7","metadata":{"cart":"42"}}`

func TestTokensList(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"tokens":[{"token":"USDT_BEP20","chain":"bsc","min_usd":"1","network_fee_usd":"0.01","amount_due_usd":"40.43","fee_note":""}]}`)
	list, err := c.Tokens.List(context.Background(), nil)
	if err != nil || len(list) != 1 || list[0].Token != "USDT_BEP20" || rec.method != http.MethodGet || rec.path != "/v1/tokens" || rec.query != "" {
		t.Fatalf("every token: %+v %v (%+v)", list, err, rec)
	}
	list, err = c.Tokens.List(context.Background(), &TokenListParams{AmountUSD: "49.90"})
	if err != nil || rec.query != "amount_usd=49.90" || list[0].AmountDueUSD != "40.43" {
		t.Fatalf("for an amount: %+v %v (query %q)", list, err, rec.query)
	}
}

func TestPaymentsSelectToken(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, testChosen)
	p, err := c.Payments.SelectToken(context.Background(), "pay_1", "XRP")
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/payments/pay_1/token" || rec.auth != "Bearer sk_test_abc" || rec.contentType != "application/json" {
		t.Errorf("request %+v", rec)
	}
	if want := map[string]any{"token": "XRP"}; !reflect.DeepEqual(rec.body, want) {
		t.Errorf("body %v, want %v", rec.body, want)
	}
	if p.Token != "XRP" || p.Chain != "xrp" || p.DepositAddress != "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe" ||
		p.DestinationTag == nil || *p.DestinationTag != 1042 || p.CryptoAmount != "20" || p.RateLocked != "0.5" ||
		p.QRPayload != "xrpl:rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe?amount=20.000000&dt=1042" ||
		!p.ExpiresAt.Equal(time.Date(2026, 10, 10, 12, 15, 0, 0, time.UTC)) || p.CheckoutToken != "ct_abc" ||
		p.CheckoutURL != "https://pay.example/ct_abc" || p.Metadata["cart"] != "42" {
		t.Errorf("payment %+v", p)
	}
	// Every field the gateway sent is in the Payment.
	var sent, got map[string]any
	back, _ := json.Marshal(p)
	if err := json.Unmarshal([]byte(testChosen), &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &got); err != nil || !reflect.DeepEqual(got, sent) {
		t.Errorf("payment %s, want %s (%v)", back, testChosen, err)
	}
	// An order id is escaped in the path, as Get does.
	if _, err := c.Payments.SelectToken(context.Background(), "ORD 7/1", "XRP"); err != nil || rec.path != "/v1/payments/ORD%207%2F1/token" {
		t.Errorf("order id: path %s, err %v", rec.path, err)
	}
}

func TestPaymentsSelectTokenChosen(t *testing.T) {
	const message = "the coin for this payment has already been chosen"
	c, _ := fakeGateway(t, http.StatusConflict, `{"error":"`+message+`"}`)
	var perr *Error
	if _, err := c.Payments.SelectToken(context.Background(), "pay_1", "XRP"); !errors.As(err, &perr) || perr.Status != http.StatusConflict || perr.Message != message {
		t.Errorf("err %v", err)
	}
}

func TestClientConstructEvent(t *testing.T) {
	fresh := sign(testSecret, time.Now().Unix(), testBody)
	c, _ := NewClient("sk_test_abc", WithBaseURL("http://x"), WithWebhookSecret(testSecret))
	if e, err := c.Webhooks.ConstructEvent([]byte(testBody), fresh); err != nil || e.ID != "evt_test_123" {
		t.Fatalf("with the client's secret: %+v %v", e, err)
	}
	// An explicit secret wins over the client's.
	other, _ := NewClient("sk_test_abc", WithBaseURL("http://x"), WithWebhookSecret("whsec_other"))
	if _, err := other.Webhooks.ConstructEvent([]byte(testBody), fresh); !errors.Is(err, ErrSignature) {
		t.Errorf("the wrong client secret: %v", err)
	}
	if e, err := other.Webhooks.ConstructEvent([]byte(testBody), fresh, testSecret); err != nil || e.ID != "evt_test_123" {
		t.Errorf("with an explicit secret: %+v %v", e, err)
	}
	// Without any secret there is nothing to verify with.
	none, _ := NewClient("sk_test_abc", WithBaseURL("http://x"))
	if _, err := none.Webhooks.ConstructEvent([]byte(testBody), fresh); !errors.Is(err, ErrSignature) || !strings.Contains(err.Error(), "no webhook secret") {
		t.Errorf("without a secret: %v", err)
	}
}

func TestConstructEventReasons(t *testing.T) {
	soon := "t=soon,v1=" + func() string {
		mac := hmac.New(sha256.New, []byte(testSecret))
		mac.Write([]byte("soon." + testBody))
		return hex.EncodeToString(mac.Sum(nil))
	}()
	cases := []struct{ name, header, secret, reason string }{
		{"no header", "", testSecret, "missing or malformed X-Webhook-Signature header"},
		{"no v1", "t=1760000000", testSecret, "missing or malformed X-Webhook-Signature header"},
		{"wrong secret", testHeader, "whsec_other", "signature does not match"},
		{"old timestamp", testHeader, testSecret, "timestamp outside the tolerance"},
		{"non-numeric timestamp", soon, testSecret, "malformed timestamp"},
	}
	for _, c := range cases {
		_, err := ConstructEvent([]byte(testBody), c.header, c.secret)
		if !errors.Is(err, ErrSignature) || err.Error() != ErrSignature.Error()+": "+c.reason {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// With the timestamp check off, a signed non-numeric timestamp is accepted.
	if _, err := ConstructEventWithTolerance([]byte(testBody), soon, testSecret, 0); err != nil {
		t.Errorf("tolerance 0: %v", err)
	}
}

func TestPaymentsList(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"payments":[],"total":0,"limit":10,"offset":20}`)
	page, err := c.Payments.List(context.Background(), &ListParams{Status: StatusConfirmed, StoreID: "sto_1", Limit: 10, Offset: 20})
	if err != nil || page.Limit != 10 || page.Offset != 20 {
		t.Fatalf("page %+v err %v", page, err)
	}
	if rec.method != http.MethodGet || rec.path != "/v1/payments" || rec.query != "limit=10&offset=20&status=confirmed&store_id=sto_1" {
		t.Errorf("request %+v", rec)
	}
	if _, err := c.Payments.List(context.Background(), nil); err != nil || rec.query != "" {
		t.Errorf("no filters: query %q err %v", rec.query, err)
	}
}

func TestDepositFields(t *testing.T) {
	// Every field the gateway sends for a deposit survives decoding.
	const deposit = `{"tx_id":"c19240","token":"USDT_TRC20","chain":"tron","amount":"49.9","usd_value":"49.9","counted":true,` +
		`"late":true,"status":"failed","seen_at":"2026-10-10T09:03:02.418Z","confirmed_at":"2026-10-10T09:04:03.77Z",` +
		`"from":"TL8wHk","block":79412538,"made_at":"2026-10-10T09:02:57Z","failed_at":"2026-10-10T15:03:02Z","fail_reason":"confirmation timeout"}`
	var d Deposit
	if err := json.Unmarshal([]byte(deposit), &d); err != nil {
		t.Fatal(err)
	}
	if d.From != "TL8wHk" || d.Block != 79412538 || d.MadeAt == nil || d.FailedAt == nil || d.FailReason != "confirmation timeout" {
		t.Errorf("deposit %+v", d)
	}
	var sent, got map[string]any
	back, _ := json.Marshal(d)
	if err := json.Unmarshal([]byte(deposit), &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &got); err != nil || len(got) != len(sent) {
		t.Errorf("deposit %s, want %s (%v)", back, deposit, err)
	}
	for k := range sent {
		if _, ok := got[k]; !ok {
			t.Errorf("deposit lost %q", k)
		}
	}
}
