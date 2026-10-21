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

const testKey = "8334dfeae25b9b200b75995e6e55cf8c3602120c1920bfef7a4ff8900b620881"

// Signed by the gateway's own signer.
const (
	testSecret  = "05bd6b428158318cc38756fa34aa60795af0fc5d9a316dcc9b060c8ca1863fc3"
	testHeader  = "t=1760000000,v1=75eb20fb34bccab4b9121285930b076e5e60ccad0a2542bd76eb56d89744faed"
	testBody    = `{"id":"b8e1f80bd70ae0784c7855a4","type":"payment.confirmed","livemode":false,"created_at":"2026-10-09T12:00:00Z","data":{"id":"040ffd5925d40e11c67b7238","order_id":"ORD-1","status":"confirmed","usd_amount":"10"}}`
	otherSecret = "bd4106123e659e259c401119d0c7ef0a221df306a5a414b6fb6a579499962d26"
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
		"wrong secret": {testBody, testHeader, otherSecret},
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
	for _, key := range []string{"", "  "} {
		if _, err := NewClient(key, WithBaseURL("http://x")); err == nil || err.Error() != "payitgo: the API key is required" {
			t.Errorf("key %q: err %v", key, err)
		}
	}
	// Without an address the client uses the release's default, or refuses.
	if c, err := NewClient(testKey); defaultBaseURL == "" && err == nil {
		t.Error("missing base URL accepted")
	} else if defaultBaseURL != "" && (err != nil || c.baseURL != strings.TrimRight(defaultBaseURL, "/")) {
		t.Errorf("default base URL not used: %v", err)
	}
	c, err := NewClient(testKey, WithBaseURL("http://x/"))
	if err != nil || c.baseURL != "http://x" {
		t.Fatalf("client %+v err %v", c, err)
	}
}

func TestIsLive(t *testing.T) {
	for _, live := range []bool{true, false} {
		var calls []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"webhook":`+strings.Replace(testWebhook, `"livemode":false`, `"livemode":`+strconv.FormatBool(live), 1)+`}`)
		}))
		c, err := NewClient(testKey, WithBaseURL(srv.URL), WithMaxRetries(0))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if got, err := c.IsLive(context.Background()); err != nil || got != live {
				t.Errorf("live %v, call %d: %v %v", live, i+1, got, err)
			}
		}
		// Asked once, with the key; the second call does not ask again.
		if want := []string{"GET /v1/webhook Bearer " + testKey}; !reflect.DeepEqual(calls, want) {
			t.Errorf("live %v: requests %q, want %q", live, calls, want)
		}
		srv.Close()
	}
}

func TestIsLiveError(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			io.WriteString(w, `{"error":"invalid API key"}`)
			return
		}
		io.WriteString(w, `{"webhook":`+testWebhook+`}`)
	}))
	defer srv.Close()
	c, _ := NewClient(testKey, WithBaseURL(srv.URL), WithMaxRetries(0))
	var perr *Error
	if _, err := c.IsLive(context.Background()); !errors.As(err, &perr) || perr.Status != http.StatusUnauthorized || perr.Message != "invalid API key" {
		t.Fatalf("err %v", err)
	}
	// An error is not kept: the next call asks again.
	status = http.StatusOK
	if live, err := c.IsLive(context.Background()); err != nil || live {
		t.Errorf("after the error: %v %v", live, err)
	}
}

// ── The webhook (IPN), against a fake gateway ───────────────────────────────

const (
	testWebhook  = `{"livemode":false,"url":"https://shop.example/payitgo/ipn","secret":"abababababababababababababababababababababababababababababababab","created_at":"2026-10-10T09:00:00Z","updated_at":"2026-10-10T09:05:00Z"}`
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
	c, err := NewClient(testKey, append([]Option{WithBaseURL(srv.URL), WithMaxRetries(0)}, opts...)...)
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
	if rec.method != http.MethodGet || rec.path != "/v1/webhook" || rec.auth != "Bearer "+testKey || rec.body != nil {
		t.Errorf("request %+v", rec)
	}
	if w.URL != "https://shop.example/payitgo/ipn" || w.Secret != strings.Repeat("ab", 32) || w.Livemode || w.UpdatedAt.IsZero() {
		t.Errorf("webhook %+v", w)
	}
}

func TestWebhooksSet(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"webhook":`+testWebhook+`}`)
	w, err := c.Webhooks.Set(context.Background(), "https://shop.example/payitgo/ipn")
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPut || rec.path != "/v1/webhook" || rec.auth != "Bearer "+testKey || rec.contentType != "application/json" {
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
	if rec.method != http.MethodDelete || rec.path != "/v1/webhook" || rec.auth != "Bearer "+testKey {
		t.Errorf("request %+v", rec)
	}
	if w.URL != "" || w.Secret != strings.Repeat("ab", 32) {
		t.Errorf("webhook %+v", w)
	}
}

func TestWebhooksTest(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusAccepted, `{"delivery":`+testDelivery+`}`)
	d, err := c.Webhooks.Test(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/webhook/test" || rec.auth != "Bearer "+testKey {
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
	`"created_at":"2026-10-10T12:00:00Z","network_fee_note":"Final in seconds. Include the destination tag: it is required.",` +
	`"qr_payload":"rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe","checkout_token":"ct_abc",` +
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
	if rec.method != http.MethodPost || rec.path != "/v1/payments/pay_1/token" || rec.auth != "Bearer "+testKey || rec.contentType != "application/json" {
		t.Errorf("request %+v", rec)
	}
	if want := map[string]any{"token": "XRP"}; !reflect.DeepEqual(rec.body, want) {
		t.Errorf("body %v, want %v", rec.body, want)
	}
	if p.Token != "XRP" || p.Chain != "xrp" || p.DepositAddress != "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe" ||
		p.DestinationTag == nil || *p.DestinationTag != 1042 || p.CryptoAmount != "20" || p.RateLocked != "0.5" ||
		p.QRPayload != "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe" ||
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
	const message = "the coin for this payment can no longer be changed"
	c, _ := fakeGateway(t, http.StatusConflict, `{"error":"`+message+`"}`)
	var perr *Error
	if _, err := c.Payments.SelectToken(context.Background(), "pay_1", "XRP"); !errors.As(err, &perr) || perr.Status != http.StatusConflict || perr.Message != message {
		t.Errorf("err %v", err)
	}
}

// testSimulated is a test payment whose simulated outcome has paid it.
const testSimulated = `{"id":"pay_2","merchant_id":"mch_1","order_id":"ORD-8","status":"detecting","livemode":false,` +
	`"simulation":{"outcome":"confirmed","state":"running"},"token":"USDT_TRC20","chain":"tron",` +
	`"deposit_address":"TPk2bpQqWRpXiaUHS9SXnZCDgvwrsuJ71o","crypto_amount":"20","received_amount":"20",` +
	`"usd_amount":"20","amount_due_usd":"20","fees_paid_by_customer":false,"network_fee":"1","network_fee_usd":"0.3",` +
	`"settlement":"custodial","received_usd":"20","rate_locked":"1",` +
	`"deposits":[{"tx_id":"8de0120d502929788a71a7b7cf60e5ef3c095cdbb05cd8a7ddabc0bc9ffa9582",` +
	`"token":"USDT_TRC20","chain":"tron","amount":"20","usd_value":"20","counted":true,"status":"pending",` +
	`"seen_at":"2026-10-10T12:00:05Z","simulated":true}],"expires_at":"2026-10-10T12:15:00Z",` +
	`"created_at":"2026-10-10T12:00:00Z","network_fee_note":"Final in about a minute.",` +
	`"qr_payload":"TPk2bpQqWRpXiaUHS9SXnZCDgvwrsuJ71o"}`

func TestPaymentsSimulate(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, testSimulated)
	p, err := c.Payments.Simulate(context.Background(), "pay_2", OutcomeConfirmed)
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/payments/pay_2/simulate" || rec.auth != "Bearer "+testKey || rec.contentType != "application/json" {
		t.Errorf("request %+v", rec)
	}
	if want := map[string]any{"outcome": "confirmed"}; !reflect.DeepEqual(rec.body, want) {
		t.Errorf("body %v, want %v", rec.body, want)
	}
	if p.Simulation == nil || p.Simulation.Outcome != OutcomeConfirmed || p.Simulation.State != SimulationRunning ||
		len(p.Deposits) != 1 || !p.Deposits[0].Simulated || len(p.Deposits[0].TxID) != 64 {
		t.Errorf("payment %+v", p)
	}
	// Every field the gateway sent is in the Payment.
	var sent, got map[string]any
	back, _ := json.Marshal(p)
	if err := json.Unmarshal([]byte(testSimulated), &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &got); err != nil || !reflect.DeepEqual(got, sent) {
		t.Errorf("payment %s, want %s (%v)", back, testSimulated, err)
	}
	// An order id is escaped in the path, as Get does.
	if _, err := c.Payments.Simulate(context.Background(), "ORD 8/1", OutcomeExpired); err != nil || rec.path != "/v1/payments/ORD%208%2F1/simulate" {
		t.Errorf("order id: path %s, err %v", rec.path, err)
	}
}

func TestPaymentsSimulateRunning(t *testing.T) {
	const message = "a simulation is already running for this payment"
	c, _ := fakeGateway(t, http.StatusConflict, `{"error":"`+message+`"}`)
	var perr *Error
	if _, err := c.Payments.Simulate(context.Background(), "pay_2", OutcomeConfirmed); !errors.As(err, &perr) || perr.Status != http.StatusConflict || perr.Message != message {
		t.Errorf("err %v", err)
	}
}

func TestPaymentsCreateSimulate(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusCreated, testSimulated)
	p, err := c.Payments.Create(context.Background(), &PaymentParams{OrderID: "ORD-8", AmountUSD: "20", Token: "USDT_TRC20", Simulate: OutcomeConfirmed})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/payments" || rec.body["simulate"] != "confirmed" {
		t.Errorf("sent %v to %s %s", rec.body, rec.method, rec.path)
	}
	if p.Simulation == nil || p.Simulation.Outcome != OutcomeConfirmed {
		t.Errorf("payment %+v", p)
	}
	// Without Simulate, the field is left out.
	if _, err := c.Payments.Create(context.Background(), &PaymentParams{OrderID: "ORD-9", AmountUSD: "20"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.body["simulate"]; ok {
		t.Errorf("sent %v", rec.body)
	}
}

// testPriced is a payment priced in euros: the gateway converted the price
// into usd_amount at price_rate.
const testPriced = `{"id":"pay_3","merchant_id":"mch_1","order_id":"ORD-10","status":"pending","livemode":false,"token":"","chain":"",` +
	`"deposit_address":"","crypto_amount":"0","received_amount":"0","usd_amount":"49.23","price_amount":"45.5",` +
	`"price_currency":"EUR","price_rate":"1.082","amount_due_usd":"49.23","fees_paid_by_customer":false,"network_fee":"0",` +
	`"network_fee_usd":"0","settlement":"custodial","received_usd":"0","rate_locked":"0","deposits":[],` +
	`"expires_at":"2026-10-10T13:00:00Z","created_at":"2026-10-10T12:00:00Z","network_fee_note":"","qr_payload":""}`

func TestPaymentsCreatePrice(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusCreated, testPriced)
	p, err := c.Payments.Create(context.Background(), &PaymentParams{OrderID: "ORD-10", PriceAmount: "45.50", PriceCurrency: "EUR"})
	if err != nil {
		t.Fatal(err)
	}
	// The price goes as decimal strings, and no amount_usd with it.
	if want := map[string]any{"order_id": "ORD-10", "price_amount": "45.50", "price_currency": "EUR"}; !reflect.DeepEqual(rec.body, want) {
		t.Errorf("body %v, want %v", rec.body, want)
	}
	if p.PriceAmount != "45.5" || p.PriceCurrency != "EUR" || p.PriceRate != "1.082" || p.USDAmount != "49.23" {
		t.Errorf("payment %+v", p)
	}
	// Every field the gateway sent is in the Payment.
	var sent, got map[string]any
	back, _ := json.Marshal(p)
	if err := json.Unmarshal([]byte(testPriced), &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &got); err != nil || !reflect.DeepEqual(got, sent) {
		t.Errorf("payment %s, want %s (%v)", back, testPriced, err)
	}
	// Priced in dollars: amount_usd as before, and none of the price fields.
	if _, err := c.Payments.Create(context.Background(), &PaymentParams{OrderID: "ORD-11", AmountUSD: "10"}); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"order_id": "ORD-11", "amount_usd": "10"}; !reflect.DeepEqual(rec.body, want) {
		t.Errorf("body %v, want %v", rec.body, want)
	}
	var dollars Payment
	if err := json.Unmarshal([]byte(testChosen), &dollars); err != nil || dollars.PriceAmount != "" || dollars.PriceCurrency != "" || dollars.PriceRate != "" {
		t.Errorf("a payment in dollars: %+v %v", dollars, err)
	}
}

func TestPaymentsCreatePriceErrors(t *testing.T) {
	cases := []struct {
		status  int
		message string
	}{
		{http.StatusBadRequest, "give amount_usd, or price_amount and price_currency: not both"},
		{http.StatusBadRequest, "price_currency must be the ISO 4217 code of a currency (EUR, GBP, NGN…)"},
		{http.StatusServiceUnavailable, "exchange rate unavailable for price_currency: try again shortly"},
	}
	for _, tc := range cases {
		reply, _ := json.Marshal(map[string]string{"error": tc.message})
		c, _ := fakeGateway(t, tc.status, string(reply))
		var perr *Error
		_, err := c.Payments.Create(context.Background(), &PaymentParams{OrderID: "ORD-12", PriceAmount: "45.50", PriceCurrency: "EUR"})
		if !errors.As(err, &perr) || perr.Status != tc.status || perr.Message != tc.message {
			t.Errorf("%d: err %v", tc.status, err)
		}
	}
}

func TestClientConstructEvent(t *testing.T) {
	fresh := sign(testSecret, time.Now().Unix(), testBody)
	c, _ := NewClient(testKey, WithBaseURL("http://x"), WithWebhookSecret(testSecret))
	if e, err := c.Webhooks.ConstructEvent([]byte(testBody), fresh); err != nil || e.ID != "b8e1f80bd70ae0784c7855a4" {
		t.Fatalf("with the client's secret: %+v %v", e, err)
	}
	// An explicit secret wins over the client's.
	other, _ := NewClient(testKey, WithBaseURL("http://x"), WithWebhookSecret(otherSecret))
	if _, err := other.Webhooks.ConstructEvent([]byte(testBody), fresh); !errors.Is(err, ErrSignature) {
		t.Errorf("the wrong client secret: %v", err)
	}
	if e, err := other.Webhooks.ConstructEvent([]byte(testBody), fresh, testSecret); err != nil || e.ID != "b8e1f80bd70ae0784c7855a4" {
		t.Errorf("with an explicit secret: %+v %v", e, err)
	}
	// Without any secret there is nothing to verify with.
	none, _ := NewClient(testKey, WithBaseURL("http://x"))
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
		{"wrong secret", testHeader, otherSecret, "signature does not match"},
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

// ── Mass payouts, against a fake gateway ─────────────────────────────────────

// testPayoutRequest is a payout request in the open list, as POST /v1/payouts
// returns it.
const testPayoutRequest = `{"id":"4e6a8c0b2d4f6a1c3e5b7d9a","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":false,` +
	`"batch_id":"7c3e5a9b1d2f4e6a8c0b2d4f","reference":"WD-7731","row":3,"recipient":{"name":"Ana Silva"},` +
	`"asset":"USDT_TRC20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg","chain":"tron",` +
	`"address":"TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi","amount":"120","send_amount":"120","platform_fee":"0.6",` +
	`"network_fee":"5.748213","fee":"6.348213","debit":"126.348213","usd_rate":"1","amount_usd":"120","status":"ready",` +
	`"errors":[],"warnings":[{"code":"new_recipient","field":"address","message":"you never paid this address","hint":"its payment waits 24 hours after funding"}],` +
	`"created_at":"2026-10-20T09:15:01.88Z","updated_at":"2026-10-20T09:15:02.4Z"}`

// testPayoutSent is a payment that went, with every field a sent payment has.
const testPayoutSent = `{"id":"8b2d4f6a1c3e5a7b9d0f2c4e","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"batch_id":"6a1f3c8e2d9b4a7051c3e8f2","reference":"payroll-2026-10-ana","row":1,` +
	`"recipient":{"name":"Ana Silva","email":"ana@example.com","note":"October salary"},"asset":"USDT_TRC20","ticker":"USDT",` +
	`"icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg","chain":"tron","address":"TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi",` +
	`"amount":"120","send_amount":"120","platform_fee":"0.6","network_fee":"5.748213","fee":"6.348213","debit":"126.348213",` +
	`"usd_rate":"1","amount_usd":"120","status":"sent","errors":[],"warnings":[],"cooling_until":"2026-10-20T09:20:00Z",` +
	`"tx_id":"3c9f1a7e5b2d8c4f6a0e9b3d7c1f5a2e8b4d0c6f9a3e7b1d5c8f2a6e0b4d9c3f",` +
	`"explorer_url":"https://tronscan.org/#/transaction/3c9f1a7e5b2d8c4f6a0e9b3d7c1f5a2e8b4d0c6f9a3e7b1d5c8f2a6e0b4d9c3f",` +
	`"sent_at":"2026-10-20T09:31:12.6Z","created_at":"2026-10-20T09:15:01.88Z","updated_at":"2026-10-20T09:31:12.6Z"}`

// testPayoutCanceled is a funded payment canceled before it was sent: its
// reference given back, its fee credited.
const testPayoutCanceled = `{"id":"9c1e3a5b7d9f2b4d6f8a0c2e","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"batch_id":"6a1f3c8e2d9b4a7051c3e8f2","released_reference":"payroll-2026-10-ben","row":2,"recipient":{},` +
	`"asset":"USDT_BEP20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_BEP20.svg","chain":"bsc",` +
	`"address":"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed","amount":"250","send_amount":"250","platform_fee":"1.25",` +
	`"network_fee":"0.01183421","fee":"1.26183421","debit":"251.26183421","usd_rate":"1","amount_usd":"250",` +
	`"status":"canceled","errors":[],"warnings":[],"fee_credit":"1.26183421",` +
	`"created_at":"2026-10-20T09:15:01.88Z","updated_at":"2026-10-20T09:20:00Z"}`

// testPayoutRemoved is a payout request removed from the list by an API key:
// canceled, its reference given back, its record kept without a batch.
const testPayoutRemoved = `{"id":"4e6a8c0b2d4f6a1c3e5b7d9a","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":false,` +
	`"released_reference":"WD-7731","row":3,"recipient":{"name":"Ana Silva"},` +
	`"asset":"USDT_TRC20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg","chain":"tron",` +
	`"address":"TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi","amount":"120","send_amount":"120","platform_fee":"0.6",` +
	`"network_fee":"5.748213","fee":"6.348213","debit":"126.348213","usd_rate":"1","amount_usd":"120","status":"canceled",` +
	`"errors":[],"warnings":[],"cancel_reason":"removed","canceled_by":"5e1a9c3d7b2f40e8a6c4d2b1","canceled_at":"2026-10-20T09:40:00Z",` +
	`"webhook_url":"https://acme.example/payitgo/withdrawals","created_at":"2026-10-20T09:15:01.88Z","updated_at":"2026-10-20T09:40:00Z"}`

// testPayoutRejected is a payment of a batch an approver rejected: canceled
// with the approver's reason, by a team member named in members.
const testPayoutRejected = `{"id":"9c1e3a5b7d9f2b4d6f8a0c2e","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"batch_id":"6a1f3c8e2d9b4a7051c3e8f2","released_reference":"payroll-2026-10-ben","row":2,"recipient":{},` +
	`"asset":"USDT_BEP20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_BEP20.svg","chain":"bsc",` +
	`"address":"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed","amount":"250","send_amount":"250","platform_fee":"1.25",` +
	`"network_fee":"0.01183421","fee":"1.26183421","debit":"251.26183421","usd_rate":"1","amount_usd":"250",` +
	`"status":"canceled","errors":[],"warnings":[],"cancel_reason":"rejected","cancel_note":"paid by bank transfer instead",` +
	`"canceled_by":"9b2e4c71d8a05f3e6c1a7b24","canceled_at":"2026-10-20T15:02:44.3Z","members":{"9b2e4c71d8a05f3e6c1a7b24":"finance@acme.example"},` +
	`"created_at":"2026-10-20T09:15:01.88Z","updated_at":"2026-10-20T15:02:44.3Z"}`

// testPayoutBatch is a submitted batch waiting for funds.
const testPayoutBatch = `{"id":"6a1f3c8e2d9b4a7051c3e8f2","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"reference":"payroll-2026-10","name":"October payroll","source":"api","inbox":false,"open":false,` +
	`"created_by":"5e1a9c3d7b2f40e8a6c4d2b1","by_api_key":true,"fee_paid_by":"merchant","convert":"convert","convert_from":"USDT_TRC20",` +
	`"webhook_url":"https://acme.example/payitgo/payouts","status":"awaiting_funds",` +
	`"waiting":{"reason":"funds","note":"top up the payout wallet: 126.348213 USDT_TRC20 on Tron (TRC-20)",` +
	`"since":"2026-10-20T09:16:40.12Z","shortfall":[{"asset":"USDT_TRC20","network":"Tron (TRC-20)",` +
	`"amount":"126.348213","address":"TMkq7dZ3vN8xR2pB6cH9sJ4fL1wY5tGaQe"}]},` +
	`"totals":[{"asset":"USDT_TRC20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg",` +
	`"chain":"tron","network":"Tron (TRC-20)","items":1,"amount":"120","send_amount":"120","platform_fee":"0.6",` +
	`"network_fee":"5.748213","fee":"6.348213","debit":"126.348213","debit_usd":"126.35","saved_usd":"0"}],` +
	`"counts":{"total":2,"invalid":1,"ready":1,"cooling":0,"queued":0,"sending":0,"sent":0,"failed":0,"canceled":0,"claimable":0,"expired":0},` +
	`"total_usd":"126.35","quote_expires_at":"2026-10-20T10:15:02.41Z","approvals_needed":1,` +
	`"approvals":[{"user_id":"9b2e4c71d8a05f3e6c1a7b24","at":"2026-10-20T09:16:30Z"}],` +
	`"submitted_by":"5e1a9c3d7b2f40e8a6c4d2b1","submitted_at":"2026-10-20T09:16:20.09Z",` +
	`"created_at":"2026-10-20T09:15:01.87Z","updated_at":"2026-10-20T09:16:40.12Z"}`

// testPayoutInbox is the open list of payout requests, a page of it.
const testPayoutInbox = `{"inbox":{"id":"7c3e5a9b1d2f4e6a8c0b2d4f","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":false,` +
	`"name":"Payout requests","source":"requests","inbox":true,"open":true,"created_by":"requests","by_api_key":false,` +
	`"fee_paid_by":"merchant","convert":"same_network","status":"quoted","totals":[{"asset":"USDT_TRC20","ticker":"USDT",` +
	`"icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg","chain":"tron","network":"Tron (TRC-20)","items":1,` +
	`"amount":"120","send_amount":"120","platform_fee":"0.6","network_fee":"5.748213","fee":"6.348213",` +
	`"debit":"126.348213","debit_usd":"126.35","saved_usd":"0"}],` +
	`"counts":{"total":3,"invalid":2,"ready":1,"cooling":0,"queued":0,"sending":0,"sent":0,"failed":0,"canceled":0,"claimable":0,"expired":0},` +
	`"total_usd":"126.35","approvals_needed":0,"approvals":[],"created_at":"2026-10-19T08:00:00Z",` +
	`"updated_at":"2026-10-20T09:15:02.4Z"},"coverage":[{"asset":"USDT_TRC20","ticker":"USDT","network":"Tron (TRC-20)",` +
	`"needed":"126.348213","spendable":"100","top_up":"26.348213","address":"TMkq7dZ3vN8xR2pB6cH9sJ4fL1wY5tGaQe"}],` +
	`"items":[` + testPayoutRequest + `],"total":3,"limit":10,"offset":20}`

// testPayoutQuote prices one payment whose recipient pays the fees: nothing
// kept, so no ids or times.
const testPayoutQuote = `{"quote":{"livemode":true,"fee_paid_by":"recipient","convert":"same_network","convert_from":"","totals":[{"asset":"USDT_BEP20","ticker":"USDT",` +
	`"icon_url":"https://api.payitgo.co/coins/USDT_BEP20.svg","chain":"bsc","network":"BNB Smart Chain (BEP-20)","items":1,` +
	`"amount":"120","send_amount":"119.38816579","platform_fee":"0.6","network_fee":"0.01183421","fee":"0.61183421",` +
	`"debit":"120","debit_usd":"120","saved_usd":"0"}],"total_usd":"120",` +
	`"counts":{"total":1,"invalid":0,"ready":1,"cooling":0,"queued":0,"sending":0,"sent":0,"failed":0,"canceled":0,"claimable":0,"expired":0}},` +
	`"items":[{"livemode":true,"row":1,"recipient":{},"asset":"USDT_BEP20","ticker":"USDT",` +
	`"icon_url":"https://api.payitgo.co/coins/USDT_BEP20.svg","chain":"bsc","address":"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",` +
	`"amount":"120","send_amount":"119.38816579","platform_fee":"0.6","network_fee":"0.01183421","fee":"0.61183421",` +
	`"debit":"120","usd_rate":"1","amount_usd":"120","status":"ready","errors":[],"warnings":[]}]}`

// testPayoutWallets is the live payout wallet: one address per network.
const testPayoutWallets = `{"wallets":[{"chain":"tron","network":"Tron","address":"TMkq7dZ3vN8xR2pB6cH9sJ4fL1wY5tGaQe",` +
	`"explorer_url":"https://tronscan.org/#/address/TMkq7dZ3vN8xR2pB6cH9sJ4fL1wY5tGaQe","livemode":true,` +
	`"assets":[{"asset":"USDT_TRC20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg",` +
	`"balance":"500","reserved":"126.348213","spendable":"373.651787","needed":"0"}]},` +
	`{"chain":"bsc","network":"BNB Smart Chain","address":"0x8F3a6B2c9D1e4F7a0B5c8D2e6F9a1B4c7D0e3F6a",` +
	`"explorer_url":"https://bscscan.com/address/0x8F3a6B2c9D1e4F7a0B5c8D2e6F9a1B4c7D0e3F6a","livemode":true,` +
	`"assets":[{"asset":"USDT_BEP20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_BEP20.svg",` +
	`"balance":"0","reserved":"0","spendable":"0","needed":"0"}],"error":"the balances could not be read just now"}]}`

// testPayoutWithdrawal is a payout wallet's withdrawal that reached the
// merchant's payout address.
const testPayoutWithdrawal = `{"id":"1d3f5a7c9e2b4d6f8a0c2e4b","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"asset":"USDT_TRC20","chain":"tron","amount":"373.651787","fee":"12.01","send_amount":"361.641787",` +
	`"address_id":"3e7a1c9f5b2d4e8a6c0f2b1d","address":"TWd4Kp9mR2xH7sN3vB6cJ8fL5qY1tZgEaU","status":"sent",` +
	`"tx_id":"7e2b9d4f1a6c3e8b5d0f2a7c4e9b1d6f3a8c5e0b2d7f4a9c1e6b3d8f5a0c2e7b",` +
	`"explorer_url":"https://tronscan.org/#/transaction/7e2b9d4f1a6c3e8b5d0f2a7c4e9b1d6f3a8c5e0b2d7f4a9c1e6b3d8f5a0c2e7b",` +
	`"requested_by":"9b2e4c71d8a05f3e6c1a7b24","created_at":"2026-10-20T11:02:15.33Z","updated_at":"2026-10-20T11:03:40Z",` +
	`"sent_at":"2026-10-20T11:03:40Z"}`

// testPayoutWithdrawalFailed is a payout wallet's withdrawal that failed: its
// money stays in the wallet.
const testPayoutWithdrawalFailed = `{"id":"2e4a6c8f0b1d3f5a7c9e1b3d","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"asset":"USDT_TRC20","chain":"tron","amount":"200","fee":"12.01","send_amount":"187.99",` +
	`"address_id":"3e7a1c9f5b2d4e8a6c0f2b1d","address":"TWd4Kp9mR2xH7sN3vB6cJ8fL5qY1tZgEaU","status":"failed",` +
	`"fail_reason":"the transfer failed on chain three times","requested_by":"9b2e4c71d8a05f3e6c1a7b24",` +
	`"created_at":"2026-10-20T12:02:15.33Z","updated_at":"2026-10-20T12:31:40Z"}`

// testPayoutClaim is a claim link, funded and emailed, waiting for its
// recipient: no address yet, two other coins offered.
const testPayoutClaim = `{"id":"2a4c6e8b0d1f3a5c7e9b1d3f","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"batch_id":"6a1f3c8e2d9b4a7051c3e8f2","reference":"prize-cup-final-2","row":4,` +
	`"recipient":{"name":"Player Two","email":"player2@example.com"},"asset":"USDT_TRC20","ticker":"USDT",` +
	`"icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg","chain":"tron","address":"","amount":"100","send_amount":"100",` +
	`"platform_fee":"0.5","network_fee":"5.748213","fee":"6.248213","debit":"106.248213","usd_rate":"1","amount_usd":"100",` +
	`"status":"claimable","errors":[],"warnings":[],"claim":{"email":"player2@example.com","assets":["USDT_BEP20","BTC"],` +
	`"expires_at":"2026-11-19T09:16:41Z","emailed":1,"last_email_at":"2026-10-20T09:16:41Z","extended":0,` +
	`"url":"https://payitgo.co/pay/claim/q3Vh8sK2mN7xR4tB9cJ6fL1wY5zGaQeU"},` +
	`"created_at":"2026-10-20T09:15:01.88Z","updated_at":"2026-10-20T09:16:41Z"}`

// testPayoutClaimed is that claim link once its recipient chose USDT on BNB
// Smart Chain: the payment is now in that coin, funded_* say what it was.
const testPayoutClaimed = `{"id":"2a4c6e8b0d1f3a5c7e9b1d3f","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"batch_id":"6a1f3c8e2d9b4a7051c3e8f2","reference":"prize-cup-final-2","row":4,` +
	`"recipient":{"name":"Player Two","email":"player2@example.com"},"asset":"USDT_BEP20","ticker":"USDT",` +
	`"icon_url":"https://api.payitgo.co/coins/USDT_BEP20.svg","chain":"bsc","address":"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",` +
	`"amount":"100","send_amount":"100","platform_fee":"0.5","network_fee":"0.01183421","fee":"0.51183421","debit":"100.51183421",` +
	`"usd_rate":"1","amount_usd":"100","status":"queued","errors":[],"warnings":[],"claim":{"email":"player2@example.com",` +
	`"assets":["USDT_BEP20","BTC"],"expires_at":"2026-11-19T09:16:41Z","claimed_at":"2026-10-21T18:02:10Z","emailed":1,` +
	`"last_email_at":"2026-10-20T09:16:41Z","extended":0,"funded_asset":"USDT_TRC20","funded_amount":"100",` +
	`"url":"https://payitgo.co/pay/claim/q3Vh8sK2mN7xR4tB9cJ6fL1wY5zGaQeU"},` +
	`"created_at":"2026-10-20T09:15:01.88Z","updated_at":"2026-10-21T18:02:10Z"}`

// testPayoutConverted is a payment of 0.01 BTC paid from the wallet's USDT on
// Tron, converted on its way, and sent.
const testPayoutConverted = `{"id":"3b5d7f9a1c3e5b7d9f1a3c5e","merchant_id":"3f8a1c27b94e05d6a2c71e90","livemode":true,` +
	`"batch_id":"6a1f3c8e2d9b4a7051c3e8f2","reference":"payroll-2026-10-dana","row":5,"recipient":{"name":"Dana Ito"},` +
	`"asset":"USDT_TRC20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg","chain":"tron",` +
	`"address":"bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq","amount":"683.4","send_amount":"683.4","platform_fee":"3.42",` +
	`"network_fee":"5.748213","fee":"13.268213","debit":"696.668213","usd_rate":"1","amount_usd":"683.4","status":"sent",` +
	`"errors":[],"warnings":[],"tx_id":"9a1c3e5b7d9f2a4c6e8b0d1f3a5c7e9b1d3f5a7c9e2b4d6f8a0c2e4b6d8f0a1c",` +
	`"convert":{"to":"BTC","ticker":"BTC","network":"Bitcoin","to_amount":"0.01","estimate":"0.01002","amount_out":"0.010011"},` +
	`"conversion_fee":"4.1","sent_at":"2026-10-20T10:02:44Z","created_at":"2026-10-20T09:15:01.88Z","updated_at":"2026-10-20T10:02:44Z"}`

// testPayoutSchedule is a monthly payroll schedule after a run, as its
// webhooks tell it (no rows).
const testPayoutSchedule = `{"id":"8d0f2b4c6e8a1c3e5d7f9b1a","livemode":true,"name":"Monthly payroll","status":"active",` +
	`"cadence":"monthly","weekday":0,"day":-1,"at":"09:00","timezone":"Africa/Harare","fee_paid_by":"merchant",` +
	`"auto_submit":true,"remind_days":3,"totals":[{"asset":"USDT_TRC20","ticker":"USDT","items":1,"amount":"1500"}],` +
	`"next_run_at":"2026-11-30T07:00:00Z","last_run_at":"2026-10-31T07:00:00Z","last_batch_id":"6a1f3c8e2d9b4a7051c3e8f2",` +
	`"last_error":"","runs":4}`

// sameJSON checks that v encodes to the JSON object sent: every field the
// gateway sent is kept, and none is added.
func sameJSON(t *testing.T, name string, v any, sent string) {
	t.Helper()
	var want, got any
	back, _ := json.Marshal(v)
	if err := json.Unmarshal([]byte(sent), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(back, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("%s: %s, want %s (%v)", name, back, sent, err)
	}
}

// Every payout method sends its method, path, query and body, with the key.
func TestPayoutRequests(t *testing.T) {
	ctx := context.Background()
	tag := uint32(7)
	items := []PayoutParams{
		{Reference: "payroll-2026-10-ana", Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120", Name: "Ana Silva", Email: "ana@example.com", Note: "October salary"},
		{Asset: "USDT_BEP20", Address: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", Amount: "250.5"},
	}
	const itemsJSON = `[{"reference":"payroll-2026-10-ana","asset":"USDT_TRC20","address":"TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi",` +
		`"amount":"120","name":"Ana Silva","email":"ana@example.com","note":"October salary"},` +
		`{"asset":"USDT_BEP20","address":"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed","amount":"250.5"}]`
	cases := []struct {
		name                string
		reply               string
		call                func(*Client) error
		method, path, query string
		body                string // "" when none is sent
	}{
		{"Payouts.Request", `{"item":` + testPayoutRequest + `}`, func(c *Client) error {
			_, err := c.Payouts.Request(ctx, &PayoutParams{Reference: "WD-7731", Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120", Name: "Ana Silva"})
			return err
		}, http.MethodPost, "/v1/payouts", "", `{"reference":"WD-7731","asset":"USDT_TRC20","address":"TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi","amount":"120","name":"Ana Silva"}`},
		{"Payouts.Request with a tag", `{"item":` + testPayoutRequest + `}`, func(c *Client) error {
			_, err := c.Payouts.Request(ctx, &PayoutParams{Asset: "XRP", Address: "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe", Tag: &tag, Amount: "20"})
			return err
		}, http.MethodPost, "/v1/payouts", "", `{"asset":"XRP","address":"rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe","tag":7,"amount":"20"}`},
		{"Payouts.Request a claim link, emailed", `{"item":` + testPayoutClaim + `}`, func(c *Client) error {
			_, err := c.Payouts.Request(ctx, &PayoutParams{Reference: "prize-cup-final-2", Asset: "USDT_TRC20", Amount: "100", Name: "Player Two",
				Email: "player2@example.com", Claim: true, ClaimAssets: []string{"USDT_BEP20", "BTC"}})
			return err
		}, http.MethodPost, "/v1/payouts", "", `{"reference":"prize-cup-final-2","asset":"USDT_TRC20","amount":"100","name":"Player Two",` +
			`"email":"player2@example.com","claim":true,"claim_assets":["USDT_BEP20","BTC"]}`},
		{"Payouts.Request with its own webhook_url", `{"item":` + testPayoutRequest + `}`, func(c *Client) error {
			_, err := c.Payouts.Request(ctx, &PayoutParams{Reference: "WD-7731", Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120",
				WebhookURL: "https://acme.example/payitgo/withdrawals"})
			return err
		}, http.MethodPost, "/v1/payouts", "", `{"reference":"WD-7731","asset":"USDT_TRC20","address":"TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi","amount":"120",` +
			`"webhook_url":"https://acme.example/payitgo/withdrawals"}`},
		{"Payouts.Request a claim link to share", `{"item":` + testPayoutClaim + `}`, func(c *Client) error {
			_, err := c.Payouts.Request(ctx, &PayoutParams{Asset: "USDT_BEP20", Amount: "25", Claim: true})
			return err
		}, http.MethodPost, "/v1/payouts", "", `{"asset":"USDT_BEP20","amount":"25","claim":true}`},
		{"Payouts.Retrieve", `{"item":` + testPayoutSent + `}`, func(c *Client) error {
			_, err := c.Payouts.Retrieve(ctx, "8b2d4f6a1c3e5a7b9d0f2c4e")
			return err
		}, http.MethodGet, "/v1/payouts/8b2d4f6a1c3e5a7b9d0f2c4e", "", ""},
		{"Payouts.Retrieve escapes the id", `{"item":` + testPayoutSent + `}`, func(c *Client) error {
			_, err := c.Payouts.Retrieve(ctx, "a b/c")
			return err
		}, http.MethodGet, "/v1/payouts/a%20b%2Fc", "", ""},
		{"Payouts.RetrieveByReference", `{"item":` + testPayoutSent + `}`, func(c *Client) error {
			_, err := c.Payouts.RetrieveByReference(ctx, "payroll 2026/10&ana")
			return err
		}, http.MethodGet, "/v1/payouts", "reference=payroll+2026%2F10%26ana", ""},
		{"Payouts.List", `{"items":[],"has_more":false}`, func(c *Client) error {
			since := time.Date(2026, 10, 20, 11, 0, 0, 500_000_000, time.FixedZone("CAT", 2*60*60))
			_, err := c.Payouts.List(ctx, &PayoutPageParams{Status: []string{PayoutItemSent, PayoutItemFailed, PayoutItemCanceled}, UpdatedSince: since,
				Cursor: "MjAyNi0xMC0yMFQwOTo0MDowMFp8NGU2YThjMGIyZDRmNmExYzNlNWI3ZDlh", Limit: 100})
			return err
		}, http.MethodGet, "/v1/payouts", "cursor=MjAyNi0xMC0yMFQwOTo0MDowMFp8NGU2YThjMGIyZDRmNmExYzNlNWI3ZDlh&limit=100" +
			"&status=sent%2Cfailed%2Ccanceled&updated_since=2026-10-20T09%3A00%3A00Z", ""},
		{"Payouts.List one status", `{"items":[],"has_more":false}`, func(c *Client) error {
			_, err := c.Payouts.List(ctx, &PayoutPageParams{Status: []string{PayoutItemCanceled}})
			return err
		}, http.MethodGet, "/v1/payouts", "status=canceled", ""},
		{"Payouts.List without params", `{"items":[],"has_more":false}`, func(c *Client) error {
			_, err := c.Payouts.List(ctx, nil)
			return err
		}, http.MethodGet, "/v1/payouts", "", ""},
		{"Payouts.List with empty params", `{"items":[],"has_more":false}`, func(c *Client) error {
			_, err := c.Payouts.List(ctx, &PayoutPageParams{})
			return err
		}, http.MethodGet, "/v1/payouts", "", ""},
		{"Payouts.Inbox", testPayoutInbox, func(c *Client) error {
			_, err := c.Payouts.Inbox(ctx, &PayoutListParams{Status: "ready,invalid", Limit: 10, Offset: 20})
			return err
		}, http.MethodGet, "/v1/payouts/inbox", "limit=10&offset=20&status=ready%2Cinvalid", ""},
		{"Payouts.Inbox without params", testPayoutInbox, func(c *Client) error {
			_, err := c.Payouts.Inbox(ctx, nil)
			return err
		}, http.MethodGet, "/v1/payouts/inbox", "", ""},
		{"Payouts.Send every valid request", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.Payouts.Send(ctx)
			return err
		}, http.MethodPost, "/v1/payouts/send", "", `{}`},
		{"Payouts.Send some", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.Payouts.Send(ctx, "4e6a8c0b2d4f6a1c3e5b7d9a", "5f7b9d1c3e5a7b2d4f6a8c0e")
			return err
		}, http.MethodPost, "/v1/payouts/send", "", `{"ids":["4e6a8c0b2d4f6a1c3e5b7d9a","5f7b9d1c3e5a7b2d4f6a8c0e"]}`},
		{"Payouts.Remove", `{"results":[{"id":"4e6a8c0b2d4f6a1c3e5b7d9a","item":` + testPayoutRemoved + `}]}`, func(c *Client) error {
			_, err := c.Payouts.Remove(ctx, "4e6a8c0b2d4f6a1c3e5b7d9a", "5f7b9d1c3e5a7b2d4f6a8c0e")
			return err
		}, http.MethodPost, "/v1/payouts/remove", "", `{"ids":["4e6a8c0b2d4f6a1c3e5b7d9a","5f7b9d1c3e5a7b2d4f6a8c0e"]}`},
		{"Payouts.Cancel", `{"item":` + testPayoutCanceled + `}`, func(c *Client) error {
			_, err := c.Payouts.Cancel(ctx, "9c1e3a5b7d9f2b4d6f8a0c2e")
			return err
		}, http.MethodDelete, "/v1/payouts/9c1e3a5b7d9f2b4d6f8a0c2e", "", ""},
		{"Payouts.Quote", testPayoutQuote, func(c *Client) error {
			_, err := c.Payouts.Quote(ctx, &PayoutQuoteParams{FeePaidBy: FeePaidByRecipient, Items: items})
			return err
		}, http.MethodPost, "/v1/payout-quotes", "", `{"fee_paid_by":"recipient","items":` + itemsJSON + `}`},
		{"Payouts.Quote converting", testPayoutQuote, func(c *Client) error {
			_, err := c.Payouts.Quote(ctx, &PayoutQuoteParams{Convert: PayoutConvert, ConvertFrom: "USDT_BEP20",
				Items: []PayoutParams{{Asset: "XRP", Address: "rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe", Tag: &tag, Amount: "40"}}})
			return err
		}, http.MethodPost, "/v1/payout-quotes", "", `{"convert":"convert","convert_from":"USDT_BEP20",` +
			`"items":[{"asset":"XRP","address":"rPT1Sjq2YGrBMTttX4GZHjKu9dyfzbpAYe","tag":7,"amount":"40"}]}`},
		{"PayoutBatches.Create", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.Create(ctx, &PayoutBatchParams{Reference: "payroll-2026-10", Name: "October payroll",
				FeePaidBy: FeePaidByMerchant, WebhookURL: "https://acme.example/payitgo/payouts", Items: items})
			return err
		}, http.MethodPost, "/v1/payout-batches", "", `{"reference":"payroll-2026-10","name":"October payroll","fee_paid_by":"merchant",` +
			`"webhook_url":"https://acme.example/payitgo/payouts","items":` + itemsJSON + `}`},
		{"PayoutBatches.Create converting", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.Create(ctx, &PayoutBatchParams{Reference: "payroll-2026-11", Convert: PayoutConvert, ConvertFrom: "USDT_TRC20",
				Items: []PayoutParams{{Asset: "BTC", Address: "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq", Amount: "0.01"}}})
			return err
		}, http.MethodPost, "/v1/payout-batches", "", `{"reference":"payroll-2026-11","convert":"convert","convert_from":"USDT_TRC20",` +
			`"items":[{"asset":"BTC","address":"bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq","amount":"0.01"}]}`},
		{"PayoutBatches.Create with a payment's own webhook_url", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.Create(ctx, &PayoutBatchParams{WebhookURL: "https://acme.example/payitgo/payouts",
				Items: []PayoutParams{{Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120", WebhookURL: "https://acme.example/payitgo/ana"}}})
			return err
		}, http.MethodPost, "/v1/payout-batches", "", `{"webhook_url":"https://acme.example/payitgo/payouts",` +
			`"items":[{"asset":"USDT_TRC20","address":"TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi","amount":"120","webhook_url":"https://acme.example/payitgo/ana"}]}`},
		{"PayoutBatches.Create with only items", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.Create(ctx, &PayoutBatchParams{Items: items[1:]})
			return err
		}, http.MethodPost, "/v1/payout-batches", "", `{"items":[{"asset":"USDT_BEP20","address":"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed","amount":"250.5"}]}`},
		{"PayoutBatches.List", `{"batches":[` + testPayoutBatch + `],"total":1,"limit":5,"offset":0}`, func(c *Client) error {
			_, err := c.PayoutBatches.List(ctx, &PayoutListParams{Status: PayoutBatchAwaitingFunds, Limit: 5})
			return err
		}, http.MethodGet, "/v1/payout-batches", "limit=5&status=awaiting_funds", ""},
		{"PayoutBatches.List without params", `{"batches":[],"total":0,"limit":50,"offset":0}`, func(c *Client) error {
			_, err := c.PayoutBatches.List(ctx, nil)
			return err
		}, http.MethodGet, "/v1/payout-batches", "", ""},
		{"PayoutBatches.Retrieve", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.Retrieve(ctx, "6a1f3c8e2d9b4a7051c3e8f2")
			return err
		}, http.MethodGet, "/v1/payout-batches/6a1f3c8e2d9b4a7051c3e8f2", "", ""},
		{"PayoutBatches.Items", `{"items":[` + testPayoutSent + `],"total":1,"limit":50,"offset":0}`, func(c *Client) error {
			_, err := c.PayoutBatches.Items(ctx, "6a1f3c8e2d9b4a7051c3e8f2", &PayoutListParams{Status: "sent,failed", Offset: 50})
			return err
		}, http.MethodGet, "/v1/payout-batches/6a1f3c8e2d9b4a7051c3e8f2/items", "offset=50&status=sent%2Cfailed", ""},
		{"PayoutBatches.Items escapes the id", `{"items":[],"total":0,"limit":50,"offset":0}`, func(c *Client) error {
			_, err := c.PayoutBatches.Items(ctx, "a b/c", nil)
			return err
		}, http.MethodGet, "/v1/payout-batches/a%20b%2Fc/items", "", ""},
		{"PayoutBatches.AddItems", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.AddItems(ctx, "6a1f3c8e2d9b4a7051c3e8f2", items)
			return err
		}, http.MethodPost, "/v1/payout-batches/6a1f3c8e2d9b4a7051c3e8f2/items", "", `{"items":` + itemsJSON + `}`},
		{"PayoutBatches.Quote", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.Quote(ctx, "6a1f3c8e2d9b4a7051c3e8f2")
			return err
		}, http.MethodPost, "/v1/payout-batches/6a1f3c8e2d9b4a7051c3e8f2/quote", "", ""},
		{"PayoutBatches.Submit", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.Submit(ctx, "6a1f3c8e2d9b4a7051c3e8f2")
			return err
		}, http.MethodPost, "/v1/payout-batches/6a1f3c8e2d9b4a7051c3e8f2/submit", "", ""},
		{"PayoutBatches.Cancel", `{"batch":` + testPayoutBatch + `}`, func(c *Client) error {
			_, err := c.PayoutBatches.Cancel(ctx, "6a1f3c8e2d9b4a7051c3e8f2")
			return err
		}, http.MethodPost, "/v1/payout-batches/6a1f3c8e2d9b4a7051c3e8f2/cancel", "", ""},
		{"PayoutWallets.List", testPayoutWallets, func(c *Client) error {
			_, err := c.PayoutWallets.List(ctx)
			return err
		}, http.MethodGet, "/v1/payout-wallets", "", ""},
		{"PayoutWallets.SimulateTopUp", testPayoutWallets, func(c *Client) error {
			_, err := c.PayoutWallets.SimulateTopUp(ctx, "USDT_TRC20", "1000")
			return err
		}, http.MethodPost, "/v1/payout-wallets/USDT_TRC20/simulate-top-up", "", `{"amount":"1000"}`},
	}
	for _, tc := range cases {
		c, rec := fakeGateway(t, http.StatusOK, tc.reply)
		if err := tc.call(c); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if rec.method != tc.method || rec.path != tc.path || rec.query != tc.query || rec.auth != "Bearer "+testKey {
			t.Errorf("%s: sent %s %s?%s (auth %q), want %s %s?%s", tc.name, rec.method, rec.path, rec.query, rec.auth, tc.method, tc.path, tc.query)
		}
		var want map[string]any
		if tc.body != "" {
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if !reflect.DeepEqual(rec.body, want) {
			t.Errorf("%s: body %v, want %v", tc.name, rec.body, want)
		}
		if (rec.contentType == "application/json") != (tc.body != "") {
			t.Errorf("%s: Content-Type %q with body %q", tc.name, rec.contentType, tc.body)
		}
	}
}

// A payout request is safe to retry: the first answer is 201, a request
// whose reference a payment carries already 200 with that payment, and both
// come back the same.
func TestPayoutsRequestRetry(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusOK} {
		c, _ := fakeGateway(t, status, `{"item":`+testPayoutRequest+`}`)
		item, err := c.Payouts.Request(context.Background(), &PayoutParams{Reference: "WD-7731", Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120"})
		if err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if item.ID != "4e6a8c0b2d4f6a1c3e5b7d9a" || item.Reference != "WD-7731" || item.Status != PayoutItemReady || item.Debit != "126.348213" ||
			len(item.Warnings) != 1 || item.Warnings[0].Code != IssueNewRecipient || item.Recipient.Name != "Ana Silva" || item.Livemode {
			t.Errorf("%d: item %+v", status, item)
		}
		sameJSON(t, "request", item, testPayoutRequest)
	}
}

// A request that fails a check is kept, invalid, with its errors.
func TestPayoutsRequestInvalid(t *testing.T) {
	invalid := strings.NewReplacer(`"status":"ready"`, `"status":"invalid"`, `"errors":[]`,
		`"errors":[{"code":"wrong_network","field":"address","message":"this address is for Tron, not BNB Smart Chain","hint":"pay it in a coin of that network, or check the address"}]`).
		Replace(testPayoutRequest)
	c, _ := fakeGateway(t, http.StatusCreated, `{"item":`+invalid+`}`)
	item, err := c.Payouts.Request(context.Background(), &PayoutParams{Asset: "USDT_BEP20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120"})
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != PayoutItemInvalid || len(item.Errors) != 1 || item.Errors[0].Code != IssueWrongNetwork || item.Errors[0].Field != "address" ||
		item.Errors[0].Hint == "" {
		t.Errorf("item %+v", item)
	}
}

func TestPayoutsRetrieve(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusOK, `{"item":`+testPayoutSent+`}`)
	for name, get := range map[string]func() (*PayoutItem, error){
		"by id": func() (*PayoutItem, error) {
			return c.Payouts.Retrieve(context.Background(), "8b2d4f6a1c3e5a7b9d0f2c4e")
		},
		"by reference": func() (*PayoutItem, error) {
			return c.Payouts.RetrieveByReference(context.Background(), "payroll-2026-10-ana")
		},
	} {
		item, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if item.Status != PayoutItemSent || len(item.TxID) != 64 || item.SentAt == nil || item.CoolingUntil == nil ||
			!item.CreatedAt.Equal(time.Date(2026, 10, 20, 9, 15, 1, 880_000_000, time.UTC)) || item.Recipient.Note != "October salary" {
			t.Errorf("%s: item %+v", name, item)
		}
		sameJSON(t, name, item, testPayoutSent)
	}
}

func TestPayoutsCancel(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusOK, `{"item":`+testPayoutCanceled+`}`)
	item, err := c.Payouts.Cancel(context.Background(), "9c1e3a5b7d9f2b4d6f8a0c2e")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != PayoutItemCanceled || item.Reference != "" || item.ReleasedReference != "payroll-2026-10-ben" || item.FeeCredit != "1.26183421" {
		t.Errorf("item %+v", item)
	}
	sameJSON(t, "canceled", item, testPayoutCanceled)

	// A request still in the list is removed: its record kept, without a
	// batch. Canceling it again answers it as it is.
	c, _ = fakeGateway(t, http.StatusOK, `{"item":`+testPayoutRemoved+`}`)
	item, err = c.Payouts.Cancel(context.Background(), "4e6a8c0b2d4f6a1c3e5b7d9a")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != PayoutItemCanceled || item.CancelReason != PayoutCancelRemoved || item.BatchID != "" || item.ReleasedReference != "WD-7731" ||
		item.CanceledBy != "5e1a9c3d7b2f40e8a6c4d2b1" || item.CanceledAt == nil || item.WebhookURL != "https://acme.example/payitgo/withdrawals" {
		t.Errorf("removed %+v", item)
	}
	sameJSON(t, "removed", item, testPayoutRemoved)
}

// Remove answers one result per id, in order: the request removed, or why
// not, as the *Error a call of its own would have returned.
func TestPayoutsRemove(t *testing.T) {
	reply := `{"results":[{"id":"4e6a8c0b2d4f6a1c3e5b7d9a","item":` + testPayoutRemoved + `},` +
		`{"id":"8b2d4f6a1c3e5a7b9d0f2c4e","error":{"status":409,"message":"not in your list of payout requests (sent already): cancel it with DELETE /v1/payouts/8b2d4f6a1c3e5a7b9d0f2c4e"}},` +
		`{"id":"0a1b2c3d4e5f60718293a4b5","error":{"status":404,"message":"not found"}}]}`
	c, rec := fakeGateway(t, http.StatusOK, reply)
	results, err := c.Payouts.Remove(context.Background(), "4e6a8c0b2d4f6a1c3e5b7d9a", "8b2d4f6a1c3e5a7b9d0f2c4e", "0a1b2c3d4e5f60718293a4b5")
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodPost || rec.path != "/v1/payouts/remove" || rec.query != "" || rec.auth != "Bearer "+testKey ||
		!reflect.DeepEqual(rec.body, map[string]any{"ids": []any{"4e6a8c0b2d4f6a1c3e5b7d9a", "8b2d4f6a1c3e5a7b9d0f2c4e", "0a1b2c3d4e5f60718293a4b5"}}) {
		t.Errorf("request %+v", rec)
	}
	if len(results) != 3 {
		t.Fatalf("results %+v", results)
	}
	if r := results[0]; r.ID != "4e6a8c0b2d4f6a1c3e5b7d9a" || r.Error != nil || r.Item == nil || r.Item.Status != PayoutItemCanceled ||
		r.Item.CancelReason != PayoutCancelRemoved || r.Item.BatchID != "" || r.Item.ReleasedReference != "WD-7731" {
		t.Errorf("removed %+v %+v", r, r.Item)
	}
	if r := results[1]; r.ID != "8b2d4f6a1c3e5a7b9d0f2c4e" || r.Item != nil || r.Error == nil || r.Error.Status != http.StatusConflict ||
		!strings.HasPrefix(r.Error.Message, "not in your list of payout requests (sent already)") {
		t.Errorf("sent already %+v %+v", r, r.Error)
	}
	if r := results[2]; r.ID != "0a1b2c3d4e5f60718293a4b5" || r.Item != nil || r.Error == nil ||
		r.Error.Error() != "payitgo: not found (HTTP 404)" {
		t.Errorf("unknown %+v %+v", r, r.Error)
	}
	var perr *Error
	if err := error(results[2].Error); !errors.As(err, &perr) || perr.Status != http.StatusNotFound {
		t.Errorf("result error %v", err)
	}
	sameJSON(t, "results", map[string]any{"results": results}, reply)
}

// Remove needs an id: none is refused before any request is sent.
func TestPayoutsRemoveNeedsIDs(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"results":[]}`)
	for _, ids := range [][]string{nil, {}} {
		results, err := c.Payouts.Remove(context.Background(), ids...)
		var perr *Error
		if !errors.As(err, &perr) || perr.Status != http.StatusBadRequest || perr.Message != "remove needs at least one payout request id" ||
			err.Error() != "payitgo: remove needs at least one payout request id (HTTP 400)" || results != nil {
			t.Errorf("%q: results %+v err %v", ids, results, err)
		}
	}
	if rec.method != "" {
		t.Errorf("sent %+v", rec)
	}
}

// RetrieveByReference needs a reference: without one, GET /v1/payouts would
// list payments, so none is refused before any request is sent.
func TestPayoutsRetrieveByReferenceNeedsReference(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"items":[],"has_more":false}`)
	for _, ref := range []string{"", "  "} {
		item, err := c.Payouts.RetrieveByReference(context.Background(), ref)
		var perr *Error
		if !errors.As(err, &perr) || perr.Status != http.StatusBadRequest || perr.Message != "reference is required" || item != nil {
			t.Errorf("%q: item %+v err %v", ref, item, err)
		}
	}
	if rec.method != "" {
		t.Errorf("sent %+v", rec)
	}
}

// List reads payments across batches, oldest change first, with the cursor
// to read on from.
func TestPayoutsList(t *testing.T) {
	reply := `{"items":[` + testPayoutSent + `,` + testPayoutRemoved + `],"has_more":true,` +
		`"next_cursor":"MjAyNi0xMC0yMFQwOTo0MDowMFp8NGU2YThjMGIyZDRmNmExYzNlNWI3ZDlh"}`
	c, rec := fakeGateway(t, http.StatusOK, reply)
	since := time.Date(2026, 10, 20, 11, 0, 0, 0, time.FixedZone("CAT", 2*60*60))
	page, err := c.Payouts.List(context.Background(), &PayoutPageParams{Status: []string{"sent", "canceled"}, UpdatedSince: since})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodGet || rec.path != "/v1/payouts" || rec.query != "status=sent%2Ccanceled&updated_since=2026-10-20T09%3A00%3A00Z" || rec.body != nil {
		t.Errorf("request %+v", rec)
	}
	if !page.HasMore || page.NextCursor != "MjAyNi0xMC0yMFQwOTo0MDowMFp8NGU2YThjMGIyZDRmNmExYzNlNWI3ZDlh" || len(page.Items) != 2 ||
		page.Items[0].Status != PayoutItemSent || page.Items[1].CancelReason != PayoutCancelRemoved || page.Items[1].BatchID != "" {
		t.Errorf("page %+v", page)
	}
	sameJSON(t, "page", page, reply)

	// Read on from the cursor.
	if _, err := c.Payouts.List(context.Background(), &PayoutPageParams{Cursor: page.NextCursor}); err != nil ||
		rec.query != "cursor=MjAyNi0xMC0yMFQwOTo0MDowMFp8NGU2YThjMGIyZDRmNmExYzNlNWI3ZDlh" {
		t.Errorf("read on: query %q err %v", rec.query, err)
	}

	// An empty page has no cursor: the one passed is kept.
	c, rec = fakeGateway(t, http.StatusOK, `{"items":[],"has_more":false}`)
	page, err = c.Payouts.List(context.Background(), nil)
	if err != nil || page.HasMore || page.NextCursor != "" || len(page.Items) != 0 || rec.query != "" {
		t.Errorf("empty page %+v (query %q) %v", page, rec.query, err)
	}
	sameJSON(t, "empty page", page, `{"items":[],"has_more":false}`)
}

// Events.List replays events as their webhooks carried them: the Event
// methods decode them the same way.
func TestEventsList(t *testing.T) {
	reply := `{"events":[{"id":"9b1e3c5a7d2f4b6e8a0c1d3f","type":"payout_item.canceled","livemode":true,` +
		`"created_at":"2026-10-20T15:02:44.312Z","data":` + testPayoutRejected + `},` +
		`{"id":"a2c4e6b8d0f1a3c5e7b9d1f3","type":"payout_wallet.withdrawal_failed","livemode":true,` +
		`"created_at":"2026-10-20T12:31:40.05Z","data":` + testPayoutWithdrawalFailed + `}],"has_more":true}`
	c, rec := fakeGateway(t, http.StatusOK, reply)
	after := time.Date(2026, 10, 20, 17, 0, 0, 0, time.FixedZone("CAT", 2*60*60))
	page, err := c.Events.List(context.Background(), &EventListParams{Types: []string{"payout_item.*", EventPayoutWalletWithdrawalFailed},
		CreatedAfter: after, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodGet || rec.path != "/v1/events" || rec.auth != "Bearer "+testKey || rec.body != nil ||
		rec.query != "created_after=2026-10-20T15%3A00%3A00Z&limit=2&type=payout_item.%2A%2Cpayout_wallet.withdrawal_failed" {
		t.Errorf("request %+v", rec)
	}
	if !page.HasMore || len(page.Events) != 2 || page.Events[0].ID != "9b1e3c5a7d2f4b6e8a0c1d3f" || page.Events[0].Type != EventPayoutItemCanceled ||
		!page.Events[0].Livemode || !page.Events[0].CreatedAt.Equal(time.Date(2026, 10, 20, 15, 2, 44, 312_000_000, time.UTC)) {
		t.Errorf("page %+v", page)
	}
	sameJSON(t, "events", page, reply)

	item, err := page.Events[0].PayoutItem()
	if err != nil || item.Status != PayoutItemCanceled || item.CancelReason != PayoutCancelRejected || item.CancelNote != "paid by bank transfer instead" ||
		item.CanceledBy != "9b2e4c71d8a05f3e6c1a7b24" || item.CanceledAt == nil || item.Members[item.CanceledBy] != "finance@acme.example" {
		t.Errorf("canceled %+v %v", item, err)
	}
	withdrawal, err := page.Events[1].PayoutWithdrawal()
	if err != nil || withdrawal.Status != "failed" || withdrawal.FailReason != "the transfer failed on chain three times" || withdrawal.SentAt != nil {
		t.Errorf("withdrawal %+v %v", withdrawal, err)
	}

	// Read on after the last event; nil params send no query.
	if _, err := c.Events.List(context.Background(), &EventListParams{After: page.Events[1].ID}); err != nil || rec.query != "after=a2c4e6b8d0f1a3c5e7b9d1f3" {
		t.Errorf("after: query %q err %v", rec.query, err)
	}
	if _, err := c.Events.List(context.Background(), nil); err != nil || rec.path != "/v1/events" || rec.query != "" {
		t.Errorf("no params: %s?%s err %v", rec.path, rec.query, err)
	}
	if _, err := c.Events.List(context.Background(), &EventListParams{Types: []string{EventPayoutItemSent}}); err != nil || rec.query != "type=payout_item.sent" {
		t.Errorf("one type: query %q err %v", rec.query, err)
	}
}

func TestPayoutsInbox(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusOK, testPayoutInbox)
	inbox, err := c.Payouts.Inbox(context.Background(), &PayoutListParams{Limit: 10, Offset: 20})
	if err != nil {
		t.Fatal(err)
	}
	if !inbox.Inbox.Inbox || !inbox.Inbox.Open || inbox.Inbox.Counts.Ready != 1 || inbox.Total != 3 || inbox.Limit != 10 || inbox.Offset != 20 ||
		len(inbox.Coverage) != 1 || inbox.Coverage[0].TopUp != "26.348213" || inbox.Coverage[0].Address == "" ||
		len(inbox.Items) != 1 || inbox.Items[0].BatchID != inbox.Inbox.ID {
		t.Errorf("inbox %+v", inbox)
	}
	sameJSON(t, "inbox", inbox, testPayoutInbox)
}

func TestPayoutsQuote(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusOK, testPayoutQuote)
	q, err := c.Payouts.Quote(context.Background(), &PayoutQuoteParams{FeePaidBy: FeePaidByRecipient,
		Items: []PayoutParams{{Asset: "USDT_BEP20", Address: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", Amount: "120"}}})
	if err != nil {
		t.Fatal(err)
	}
	if q.Quote.FeePaidBy != FeePaidByRecipient || q.Quote.Convert != PayoutSameNetwork || q.Quote.ConvertFrom != "" || q.Quote.TotalUSD != "120" || len(q.Quote.Totals) != 1 || q.Quote.Totals[0].SendAmount != "119.38816579" ||
		q.Quote.Counts.Ready != 1 || len(q.Items) != 1 || q.Items[0].ID != "" || !q.Items[0].CreatedAt.IsZero() || q.Items[0].Fee != "0.61183421" {
		t.Errorf("quote %+v", q)
	}
	var sent struct {
		Quote json.RawMessage `json:"quote"`
	}
	json.Unmarshal([]byte(testPayoutQuote), &sent)
	sameJSON(t, "quote", q.Quote, string(sent.Quote))
}

// Creating a batch is safe to retry: 201 for a new one, 200 for the batch
// its reference already names, both the same.
func TestPayoutBatchesCreate(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusOK} {
		c, _ := fakeGateway(t, status, `{"batch":`+testPayoutBatch+`}`)
		b, err := c.PayoutBatches.Create(context.Background(), &PayoutBatchParams{Reference: "payroll-2026-10",
			Items: []PayoutParams{{Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120"}}})
		if err != nil {
			t.Fatalf("%d: %v", status, err)
		}
		if b.ID != "6a1f3c8e2d9b4a7051c3e8f2" || b.Reference != "payroll-2026-10" || b.Status != PayoutBatchAwaitingFunds {
			t.Errorf("%d: batch %+v", status, b)
		}
		sameJSON(t, "batch", b, testPayoutBatch)
	}
}

func TestPayoutBatchFields(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusOK, `{"batch":`+testPayoutBatch+`}`)
	b, err := c.PayoutBatches.Submit(context.Background(), "6a1f3c8e2d9b4a7051c3e8f2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Waiting == nil || b.Waiting.Reason != PayoutWaitFunds || len(b.Waiting.Shortfall) != 1 || b.Waiting.Shortfall[0].Amount != "126.348213" ||
		b.Waiting.Shortfall[0].Address != "TMkq7dZ3vN8xR2pB6cH9sJ4fL1wY5tGaQe" || len(b.Totals) != 1 || b.Totals[0].Items != 1 ||
		b.Totals[0].DebitUSD != "126.35" || b.Counts.Total != 2 || b.Counts.Invalid != 1 || b.TotalUSD != "126.35" ||
		b.QuoteExpiresAt == nil || b.ApprovalsNeeded != 1 || len(b.Approvals) != 1 || b.SubmittedAt == nil || !b.ByAPIKey ||
		b.FeePaidBy != FeePaidByMerchant || b.WebhookURL == "" || b.Rejection != nil || b.Convert != PayoutConvert || b.ConvertFrom != "USDT_TRC20" {
		t.Errorf("batch %+v", b)
	}
}

func TestPayoutBatchesLists(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusOK, `{"batches":[`+testPayoutBatch+`],"total":7,"limit":1,"offset":3}`)
	list, err := c.PayoutBatches.List(context.Background(), &PayoutListParams{Limit: 1, Offset: 3})
	if err != nil || len(list.Batches) != 1 || list.Batches[0].ID != "6a1f3c8e2d9b4a7051c3e8f2" || list.Total != 7 || list.Limit != 1 || list.Offset != 3 {
		t.Errorf("batches %+v %v", list, err)
	}
	c, _ = fakeGateway(t, http.StatusOK, `{"items":[`+testPayoutSent+`,`+testPayoutCanceled+`],"total":2,"limit":50,"offset":0}`)
	items, err := c.PayoutBatches.Items(context.Background(), "6a1f3c8e2d9b4a7051c3e8f2", nil)
	if err != nil || len(items.Items) != 2 || items.Items[0].Status != PayoutItemSent || items.Items[1].Status != PayoutItemCanceled ||
		items.Total != 2 || items.Limit != 50 {
		t.Errorf("items %+v %v", items, err)
	}
}

func TestPayoutWallets(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusOK, testPayoutWallets)
	wallets, err := c.PayoutWallets.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(wallets) != 2 || wallets[0].Chain != "tron" || wallets[0].Address != "TMkq7dZ3vN8xR2pB6cH9sJ4fL1wY5tGaQe" ||
		len(wallets[0].Assets) != 1 || wallets[0].Assets[0].Spendable != "373.651787" || wallets[1].Error == "" {
		t.Errorf("wallets %+v", wallets)
	}
	sameJSON(t, "wallets", map[string]any{"wallets": wallets}, testPayoutWallets)

	const topped = `{"wallets":[{"chain":"tron","network":"Tron","address":"TQn9Y2khEsLJW1ChVWFMSMeRDow5KcbLSE","livemode":false,` +
		`"assets":[{"asset":"USDT_TRC20","ticker":"USDT","icon_url":"https://api.payitgo.co/coins/USDT_TRC20.svg","balance":"1000",` +
		`"reserved":"0","spendable":"1000","needed":"0"}]}]}`
	c, _ = fakeGateway(t, http.StatusOK, topped)
	wallets, err = c.PayoutWallets.SimulateTopUp(context.Background(), "USDT_TRC20", "1000")
	if err != nil || len(wallets) != 1 || wallets[0].Livemode || wallets[0].Assets[0].Balance != "1000" {
		t.Errorf("topped up: %+v %v", wallets, err)
	}
	sameJSON(t, "topped up", map[string]any{"wallets": wallets}, topped)
}

// A claim link comes back without an address, with its link; once claimed
// in another coin, it says what it was funded with.
func TestPayoutClaimLinks(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusCreated, `{"item":`+testPayoutClaim+`}`)
	item, err := c.Payouts.Request(context.Background(), &PayoutParams{Asset: "USDT_TRC20", Amount: "100", Email: "player2@example.com", Claim: true})
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != PayoutItemClaimable || item.Address != "" || item.Claim == nil || item.Claim.Email != "player2@example.com" ||
		!reflect.DeepEqual(item.Claim.Assets, []string{"USDT_BEP20", "BTC"}) || item.Claim.ExpiresAt == nil || item.Claim.ClaimedAt != nil ||
		item.Claim.Emailed != 1 || item.Claim.LastEmailAt == nil || item.Claim.URL != "https://payitgo.co/pay/claim/q3Vh8sK2mN7xR4tB9cJ6fL1wY5zGaQeU" ||
		item.Claim.FundedAsset != "" {
		t.Errorf("claim link %+v %+v", item, item.Claim)
	}
	sameJSON(t, "claim link", item, testPayoutClaim)

	c, _ = fakeGateway(t, http.StatusOK, `{"item":`+testPayoutClaimed+`}`)
	item, err = c.Payouts.Retrieve(context.Background(), "2a4c6e8b0d1f3a5c7e9b1d3f")
	if err != nil {
		t.Fatal(err)
	}
	if item.Status != PayoutItemQueued || item.Asset != "USDT_BEP20" || item.Address == "" || item.Claim == nil || item.Claim.ClaimedAt == nil ||
		item.Claim.FundedAsset != "USDT_TRC20" || item.Claim.FundedAmount != "100" {
		t.Errorf("claimed %+v %+v", item, item.Claim)
	}
	sameJSON(t, "claimed", item, testPayoutClaimed)
}

// A converted payment is in the wallet's coin, with what it converts into.
func TestPayoutConverted(t *testing.T) {
	c, _ := fakeGateway(t, http.StatusOK, `{"item":`+testPayoutConverted+`}`)
	item, err := c.Payouts.RetrieveByReference(context.Background(), "payroll-2026-10-dana")
	if err != nil {
		t.Fatal(err)
	}
	if item.Asset != "USDT_TRC20" || item.Chain != "tron" || item.ConversionFee != "4.1" || item.Convert == nil || item.Convert.To != "BTC" ||
		item.Convert.Ticker != "BTC" || item.Convert.Network != "Bitcoin" || item.Convert.ToAmount != "0.01" || item.Convert.Estimate != "0.01002" ||
		item.Convert.AmountOut != "0.010011" || item.Claim != nil {
		t.Errorf("converted %+v %+v", item, item.Convert)
	}
	sameJSON(t, "converted", item, testPayoutConverted)

	// A payment paid in its own coin has neither.
	var plain PayoutItem
	if err := json.Unmarshal([]byte(testPayoutSent), &plain); err != nil || plain.Convert != nil || plain.ConversionFee != "" || plain.Claim != nil {
		t.Errorf("plain %+v %v", plain, err)
	}
}

func TestPayoutErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		status  int
		message string
		call    func(*Client) error
	}{
		{http.StatusForbidden, "this api key cannot make payouts: turn payouts on for it in the dashboard", func(c *Client) error {
			_, err := c.Payouts.Request(ctx, &PayoutParams{Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120"})
			return err
		}},
		{http.StatusBadRequest, "no valid payout request to send", func(c *Client) error {
			_, err := c.Payouts.Send(ctx)
			return err
		}},
		{http.StatusNotFound, "not found", func(c *Client) error {
			_, err := c.PayoutBatches.Retrieve(ctx, "6a1f3c8e2d9b4a7051c3e8f2")
			return err
		}},
		{http.StatusConflict, "its prices expired: get a new quote first", func(c *Client) error {
			_, err := c.PayoutBatches.Submit(ctx, "6a1f3c8e2d9b4a7051c3e8f2")
			return err
		}},
		{http.StatusBadRequest, "only a test wallet is topped up on paper: use a test key", func(c *Client) error {
			_, err := c.PayoutWallets.SimulateTopUp(ctx, "USDT_TRC20", "1000")
			return err
		}},
		{http.StatusBadRequest, "remove between 1 and 1000 payout requests at a time", func(c *Client) error {
			_, err := c.Payouts.Remove(ctx, "")
			return err
		}},
		{http.StatusBadRequest, "cursor is not one this API gave", func(c *Client) error {
			_, err := c.Payouts.List(ctx, &PayoutPageParams{Cursor: "nope"})
			return err
		}},
		{http.StatusForbidden, "a storefront's key cannot read the account's events: use an account key", func(c *Client) error {
			_, err := c.Events.List(ctx, nil)
			return err
		}},
		{http.StatusBadRequest, "after is not an event of this mode", func(c *Client) error {
			_, err := c.Events.List(ctx, &EventListParams{After: "0a1b2c3d4e5f60718293a4b5"})
			return err
		}},
	}
	for _, tc := range cases {
		reply, _ := json.Marshal(map[string]string{"error": tc.message})
		c, _ := fakeGateway(t, tc.status, string(reply))
		var perr *Error
		if err := tc.call(c); !errors.As(err, &perr) || perr.Status != tc.status || perr.Message != tc.message {
			t.Errorf("%d %s: err %v", tc.status, tc.message, err)
		}
	}
}

// Payout events carry a batch, a payment or a withdrawal, each decoded whole.
func TestPayoutEvents(t *testing.T) {
	rejected := strings.Replace(testPayoutBatch, `"submitted_by"`,
		`"rejection":{"by":"9b2e4c71d8a05f3e6c1a7b24","at":"2026-10-20T09:40:00Z","reason":"wrong amounts"},"submitted_by"`, 1)
	failed := strings.NewReplacer(`"status":"sent"`, `"status":"failed"`, `"sent_at":"2026-10-20T09:31:12.6Z",`,
		`"fail_reason":"the transfer failed on chain three times",`).Replace(testPayoutSent)
	expired := strings.NewReplacer(`"status":"claimable"`, `"status":"expired"`).Replace(testPayoutClaim)
	// A request sent from the list into a batch of its own, with its own
	// webhook_url; then back in the list, no longer valid.
	processing := strings.NewReplacer(`"batch_id":"7c3e5a9b1d2f4e6a8c0b2d4f"`, `"batch_id":"6a1f3c8e2d9b4a7051c3e8f2"`,
		`"created_at"`, `"webhook_url":"https://acme.example/payitgo/withdrawals","created_at"`).Replace(testPayoutRequest)
	returned := strings.NewReplacer(`"status":"ready"`, `"status":"invalid"`, `"errors":[]`,
		`"errors":[{"code":"below_minimum","field":"amount","message":"worth less than the smallest payment in USDT_TRC20 right now"}]`).
		Replace(testPayoutRequest)
	// A schedule that has not run yet: no last run, batch or error.
	first := strings.NewReplacer(`"last_run_at":"2026-10-31T07:00:00Z","last_batch_id":"6a1f3c8e2d9b4a7051c3e8f2"`,
		`"last_run_at":null,"last_batch_id":""`, `"runs":4`, `"runs":0`).Replace(testPayoutSchedule)
	funding := `{"schedule":` + first + `,"shortfall":[{"asset":"USDT_TRC20","network":"Tron (TRC-20)",` +
		`"amount":"1506.25","address":"TMkq7dZ3vN8xR2pB6cH9sJ4fL1wY5tGaQe"}]}`
	cases := []struct {
		typ, data string
		decode    func(*Event) (any, error)
	}{
		{EventPayoutBatchRejected, rejected, func(e *Event) (any, error) { return e.PayoutBatch() }},
		{EventPayoutItemProcessing, processing, func(e *Event) (any, error) { return e.PayoutItem() }},
		{EventPayoutItemReturned, returned, func(e *Event) (any, error) { return e.PayoutItem() }},
		{EventPayoutItemFailed, failed, func(e *Event) (any, error) { return e.PayoutItem() }},
		{EventPayoutItemCanceled, testPayoutRejected, func(e *Event) (any, error) { return e.PayoutItem() }},
		{EventPayoutWalletWithdrawn, testPayoutWithdrawal, func(e *Event) (any, error) { return e.PayoutWithdrawal() }},
		{EventPayoutWalletWithdrawalFailed, testPayoutWithdrawalFailed, func(e *Event) (any, error) { return e.PayoutWithdrawal() }},
		{EventPayoutItemClaimed, testPayoutClaimed, func(e *Event) (any, error) { return e.PayoutItem() }},
		{EventPayoutItemClaimExpired, expired, func(e *Event) (any, error) { return e.PayoutItem() }},
		{EventPayoutScheduleRun, testPayoutSchedule, func(e *Event) (any, error) { return e.PayoutSchedule() }},
		{EventPayoutScheduleFundingNeeded, funding, func(e *Event) (any, error) { return e.PayoutScheduleFunding() }},
	}
	for _, tc := range cases {
		body := `{"id":"5c0e1f9a2b7d4e3c8a6f1b2d","type":"` + tc.typ + `","livemode":true,"created_at":"2026-10-20T09:40:00Z","data":` + tc.data + `}`
		e, err := ConstructEvent([]byte(body), sign(testSecret, time.Now().Unix(), body), testSecret)
		if err != nil || e.Type != tc.typ {
			t.Fatalf("%s: %+v %v", tc.typ, e, err)
		}
		v, err := tc.decode(e)
		if err != nil {
			t.Fatalf("%s: %v", tc.typ, err)
		}
		sameJSON(t, tc.typ, v, tc.data)
		switch d := v.(type) {
		case *PayoutBatch:
			if d.Rejection == nil || d.Rejection.Reason != "wrong amounts" {
				t.Errorf("%s: %+v", tc.typ, d)
			}
		case *PayoutItem:
			bad := d.Status != map[string]string{EventPayoutItemProcessing: PayoutItemReady, EventPayoutItemReturned: PayoutItemInvalid,
				EventPayoutItemFailed: PayoutItemFailed, EventPayoutItemCanceled: PayoutItemCanceled, EventPayoutItemClaimed: PayoutItemQueued,
				EventPayoutItemClaimExpired: PayoutItemExpired}[tc.typ]
			switch tc.typ {
			case EventPayoutItemProcessing:
				bad = bad || d.BatchID != "6a1f3c8e2d9b4a7051c3e8f2" || d.WebhookURL != "https://acme.example/payitgo/withdrawals"
			case EventPayoutItemReturned:
				bad = bad || d.BatchID != "7c3e5a9b1d2f4e6a8c0b2d4f" || len(d.Errors) != 1 || d.Errors[0].Code != IssueBelowMinimum
			case EventPayoutItemFailed:
				bad = bad || d.FailReason == ""
			case EventPayoutItemCanceled:
				bad = bad || d.CancelReason != PayoutCancelRejected || d.CancelNote == "" || d.CanceledAt == nil ||
					d.Members[d.CanceledBy] != "finance@acme.example" || d.ReleasedReference == ""
			default:
				bad = bad || d.Claim == nil
			}
			if bad {
				t.Errorf("%s: %+v", tc.typ, d)
			}
		case *PayoutSchedule:
			if d.Cadence != "monthly" || d.Day != -1 || d.NextRunAt == nil || d.LastRunAt == nil || d.LastBatchID != "6a1f3c8e2d9b4a7051c3e8f2" ||
				!d.Livemode || len(d.Totals) != 1 || d.Totals[0].Amount != "1500" || d.Runs != 4 {
				t.Errorf("%s: %+v", tc.typ, d)
			}
		case *PayoutScheduleFunding:
			if d.Schedule.ID != "8d0f2b4c6e8a1c3e5d7f9b1a" || d.Schedule.LastRunAt != nil || d.Schedule.LastBatchID != "" ||
				len(d.Shortfall) != 1 || d.Shortfall[0].Amount != "1506.25" ||
				d.Shortfall[0].Address != "TMkq7dZ3vN8xR2pB6cH9sJ4fL1wY5tGaQe" {
				t.Errorf("%s: %+v", tc.typ, d)
			}
		case *PayoutWithdrawal:
			if tc.typ == EventPayoutWalletWithdrawn && (d.Status != "sent" || d.SendAmount != "361.641787" || d.SentAt == nil || d.FailReason != "") ||
				tc.typ == EventPayoutWalletWithdrawalFailed && (d.Status != "failed" || d.FailReason == "" || d.SentAt != nil || d.TxID != "") {
				t.Errorf("%s: %+v", tc.typ, d)
			}
		}
	}
	got := []string{EventPaymentPartiallyPaid, EventPaymentDetecting, EventPaymentConfirmed, EventPaymentUnderpaid,
		EventPaymentExpired, EventPaymentFailed, EventPaymentWrongAsset, EventPaymentLateDeposit,
		EventPayoutApproved, EventPayoutRejected, EventPayoutSent, EventSettlementCompleted, EventSettlementFailed,
		EventPayoutBatchQuoted, EventPayoutBatchPendingApproval, EventPayoutBatchApproved, EventPayoutBatchRejected,
		EventPayoutBatchAwaitingFunds, EventPayoutBatchProcessing, EventPayoutBatchCompleted, EventPayoutBatchCanceled,
		EventPayoutItemProcessing, EventPayoutItemReturned, EventPayoutItemSent, EventPayoutItemFailed, EventPayoutItemCanceled,
		EventPayoutItemClaimed, EventPayoutItemClaimExpired, EventPayoutWalletWithdrawn, EventPayoutWalletWithdrawalFailed,
		EventPayoutScheduleRun, EventPayoutScheduleFundingNeeded, EventPing}
	want := []string{"payment.partially_paid", "payment.detecting", "payment.confirmed", "payment.underpaid",
		"payment.expired", "payment.failed", "payment.wrong_asset", "payment.late_deposit",
		"payout.approved", "payout.rejected", "payout.sent", "settlement.completed", "settlement.failed",
		"payout_batch.quoted", "payout_batch.pending_approval", "payout_batch.approved", "payout_batch.rejected",
		"payout_batch.awaiting_funds", "payout_batch.processing", "payout_batch.completed", "payout_batch.canceled",
		"payout_item.processing", "payout_item.returned", "payout_item.sent", "payout_item.failed", "payout_item.canceled",
		"payout_item.claimed", "payout_item.claim_expired", "payout_wallet.withdrawn", "payout_wallet.withdrawal_failed",
		"payout_schedule.run", "payout_schedule.funding_needed", "ping"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("event types %q, want %q", got, want)
	}
}

func TestVersion(t *testing.T) {
	c, rec := fakeGateway(t, http.StatusOK, `{"wallets":[]}`)
	ua := ""
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		ua = r.Header.Get("User-Agent")
		return http.DefaultTransport.RoundTrip(r)
	})
	if _, err := c.PayoutWallets.List(context.Background()); err != nil || Version == "" || ua != "payitgo-go/"+Version || rec.path != "/v1/payout-wallets" {
		t.Errorf("version %s, User-Agent %q, err %v", Version, ua, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
