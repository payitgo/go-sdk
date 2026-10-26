// Package payitgo is the Go client for the payitgo crypto payment gateway.
//
//	client, err := payitgo.NewClient(os.Getenv("PAYITGO_KEY"))
//	payment, err := client.Payments.Create(ctx, &payitgo.PaymentParams{
//		OrderID: "ORD-1001", Amount: "10", Token: "USDT_TRC20",
//	})
//	// send the customer to payment.CheckoutURL; fulfil on the payment.confirmed webhook
//
// Mass payouts pay your own clients from your payout wallet:
//
//	item, err := client.Payouts.Request(ctx, &payitgo.PayoutParams{
//		Reference: "WD-7731", Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "120",
//	})
//	batch, err := client.Payouts.Send(ctx) // every valid request in the open list
//
// Use the client from your server only: the API key is a secret.
package payitgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is this library's version, sent in the User-Agent.
const Version = "0.7.3"

// defaultBaseURL is used when neither WithBaseURL nor PAYITGO_BASE_URL is set.
const defaultBaseURL = "https://api.payitgo.co"

// Payment statuses.
const (
	StatusPending       = "pending"
	StatusPartiallyPaid = "partially_paid"
	StatusDetecting     = "detecting"
	StatusConfirmed     = "confirmed"
	StatusUnderpaid     = "underpaid"
	StatusExpired       = "expired"
	StatusFailed        = "failed"
)

// Simulated outcomes, for a test payment's PaymentParams.Simulate or
// PaymentsService.Simulate.
const (
	OutcomeConfirmed     = "confirmed"
	OutcomeOverpaid      = "overpaid"
	OutcomePartiallyPaid = "partially_paid"
	OutcomeUnderpaid     = "underpaid"
	OutcomeExpired       = "expired"
	OutcomeFailed        = "failed"
	OutcomeWrongAsset    = "wrong_asset"
	OutcomePaidLate      = "paid_late"
	OutcomePaidTwice     = "paid_twice"
)

// Simulation states.
const (
	SimulationRequested = "requested" // asked for on create, waiting for the coin
	SimulationRunning   = "running"
	SimulationDone      = "done"
)

// Simulation is a test payment's simulated outcome and how far it is.
type Simulation struct {
	Outcome string `json:"outcome"` // an Outcome… constant
	State   string `json:"state"`   // a Simulation… constant
}

// Deposit is one transfer to a payment's address.
type Deposit struct {
	TxID  string `json:"tx_id"`
	Token string `json:"token"` // the asset that arrived
	Chain string `json:"chain"`
	// Amount is in Token; Value is its value in the payment's Currency.
	Amount  string `json:"amount"`
	Value   string `json:"value"`
	Counted bool   `json:"counted"`        // counts toward the payment
	Late    bool   `json:"late,omitempty"` // arrived after the payment had closed
	// Status is pending (confirming), confirmed (final and credited) or failed.
	Status      string     `json:"status"`
	SeenAt      time.Time  `json:"seen_at"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	// From is the sender's address when the network reports it (for a
	// Bitcoin-like coin, the first input's): where a refund would go. To is
	// the address it was sent to: the payment's, or a previous coin's.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Block is the block (on XRP, the ledger) that holds the transfer, and
	// MadeAt that block's time.
	Block  int64      `json:"block,omitempty"`
	MadeAt *time.Time `json:"made_at,omitempty"`
	// FailedAt and FailReason ("transaction not found on chain" or
	// "confirmation timeout") say when and why a deposit never became final.
	FailedAt   *time.Time `json:"failed_at,omitempty"`
	FailReason string     `json:"fail_reason,omitempty"`
	// Simulated is true for a deposit a test payment's simulation played: its
	// TxID names no transaction on any chain.
	Simulated bool `json:"simulated,omitempty"`
}

// PreviousCoin is a coin chosen for a payment before its current one, when
// the customer changed it. Its address is still watched.
type PreviousCoin struct {
	Token          string  `json:"token"`
	Chain          string  `json:"chain"`
	DepositAddress string  `json:"deposit_address"`
	DestinationTag *uint32 `json:"destination_tag,omitempty"`
	CryptoAmount   string  `json:"crypto_amount"`
	RateLocked     string  `json:"rate_locked"`
}

// Payment is a payment as the API returns it. Amounts are decimal strings.
type Payment struct {
	ID         string `json:"id"`
	MerchantID string `json:"merchant_id,omitempty"`
	StoreID    string `json:"store_id,omitempty"`
	OrderID    string `json:"order_id"`
	Status     string `json:"status"`
	Livemode   bool   `json:"livemode"`
	// Simulation is set on a test payment that has a simulated outcome.
	Simulation *Simulation `json:"simulation,omitempty"`
	// Token and Chain are empty until the customer chooses the coin, from
	// TokenOptions, which stay while the coin can still change.
	// PreviousCoins are the coins chosen before it.
	Token          string         `json:"token"`
	Chain          string         `json:"chain"`
	TokenOptions   []TokenInfo    `json:"token_options,omitempty"`
	PreviousCoins  []PreviousCoin `json:"previous_coins,omitempty"`
	DepositAddress string         `json:"deposit_address"`
	DestinationTag *uint32        `json:"destination_tag,omitempty"`
	// CryptoAmount is what the customer sends, in Token; CryptoReceived what
	// has arrived in it.
	CryptoAmount   string `json:"crypto_amount"`
	CryptoReceived string `json:"crypto_received"`
	// Currency is what the payment was priced in: an ISO 4217 code, upper
	// case ("USD" unless PaymentParams.Currency gave another), or the coin of
	// a payment link priced in a coin. Every figure below is in it: Amount is
	// your price; AmountDue what the customer pays, the price plus the fees
	// when you pass them on; AmountReceived what has arrived so far;
	// AmountExtra the part of AmountDue paid only to reach the exchange's
	// minimum, which you receive (empty when there is none); RateLocked the
	// coin's price, locked when the coin was chosen.
	Currency           string            `json:"currency"`
	Amount             string            `json:"amount"`
	AmountDue          string            `json:"amount_due"`
	AmountReceived     string            `json:"amount_received"`
	AmountExtra        string            `json:"amount_extra,omitempty"`
	FeesPaidByCustomer bool              `json:"fees_paid_by_customer"`
	Settlement         string            `json:"settlement"` // "custodial" (your balance) or "direct" (your payout address)
	RateLocked         string            `json:"rate_locked"`
	TxID               string            `json:"tx_id,omitempty"`
	Overpaid           bool              `json:"overpaid,omitempty"`
	PaidLate           bool              `json:"paid_late,omitempty"`
	Deposits           []Deposit         `json:"deposits"`
	ExpiresAt          time.Time         `json:"expires_at"`
	CreatedAt          time.Time         `json:"created_at"`
	ConfirmedAt        *time.Time        `json:"confirmed_at,omitempty"`
	NetworkFeeNote     string            `json:"network_fee_note"`
	QRPayload          string            `json:"qr_payload"`
	CheckoutToken      string            `json:"checkout_token,omitempty"`
	CheckoutURL        string            `json:"checkout_url,omitempty"`
	Description        string            `json:"description,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	CustomerEmail      string            `json:"customer_email,omitempty"`
	SuccessURL         string            `json:"success_url,omitempty"`
	CancelURL          string            `json:"cancel_url,omitempty"`
	WebhookURL         string            `json:"webhook_url,omitempty"` // where its events go instead of the webhook's URL
}

// PaymentParams creates a payment. OrderID is required, and Amount, the
// price in Currency: an ISO 4217 code such as "EUR", "GBP" or "NGN" (any
// case), US dollars when empty. Leave Token empty to let the customer choose the coin on the
// checkout page, from Tokens (default: every token the amount reaches the
// minimum of). WebhookURL (https) gets the payment's events instead of the
// webhook's URL, signed with the same secret; empty means the client's
// WithWebhookURL. Simulate (an Outcome… constant, test keys only) plays that
// outcome on the test payment once it has its coin.
type PaymentParams struct {
	OrderID       string            `json:"order_id"`
	Amount        string            `json:"amount"`             // decimal, e.g. "19.99"
	Currency      string            `json:"currency,omitempty"` // e.g. "EUR"; US dollars when empty
	Token         string            `json:"token,omitempty"`
	Tokens        []string          `json:"tokens,omitempty"`
	Description   string            `json:"description,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	CustomerEmail string            `json:"customer_email,omitempty"`
	SuccessURL    string            `json:"success_url,omitempty"`
	CancelURL     string            `json:"cancel_url,omitempty"`
	WebhookURL    string            `json:"webhook_url,omitempty"`
	Simulate      string            `json:"simulate,omitempty"`
}

// ListParams narrows a payment listing; zero values are left out. StoreID
// keeps the payments of one store (ignored for a storefront's key, which
// sees its own).
type ListParams struct {
	Status  string
	Chain   string
	Token   string
	StoreID string
	Limit   int
	Offset  int
}

// PaymentList is one page of payments.
type PaymentList struct {
	Payments []Payment `json:"payments"`
	Total    int       `json:"total"`
	Limit    int       `json:"limit"`
	Offset   int       `json:"offset"`
}

// TokenInfo is a token a customer can pay with. Its figures are in the
// listing's currency: a payment's own for its TokenOptions, the one
// TokensService.List was asked for (US dollars without one). Minimum and
// NetworkFee move with the network's fees.
type TokenInfo struct {
	Token      string `json:"token"`
	Chain      string `json:"chain"`
	Minimum    string `json:"minimum"`
	NetworkFee string `json:"network_fee"`
	// AmountDue, in a payment's TokenOptions (or a token listing with an
	// amount) when fees are passed on, is what the customer pays in this
	// token; AmountExtra the part of it paid only to reach the exchange's
	// minimum, which the merchant receives.
	AmountDue   string `json:"amount_due,omitempty"`
	AmountExtra string `json:"amount_extra,omitempty"`
	FeeNote     string `json:"fee_note"`
}

// Error is a request the API refused. Status is the HTTP status, or 0 for a
// network error. A call the library refuses before sending it (an empty
// reference or list of ids) is one too, with Status 400. It is also why one
// of several ids was not done (PayoutRemoveResult.Error): what a call of its
// own would have answered.
type Error struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return "payitgo: " + e.Message
	}
	return fmt.Sprintf("payitgo: %s (HTTP %d)", e.Message, e.Status)
}

// Client talks to the payitgo API with one secret key. IsLive says whether it
// is a live key or a test key.
type Client struct {
	Payments      *PaymentsService
	Tokens        *TokensService
	Checkout      *CheckoutService
	Webhooks      *WebhooksService
	Payouts       *PayoutsService
	PayoutBatches *PayoutBatchesService
	PayoutWallets *PayoutWalletsService
	Events        *EventsService

	apiKey        string
	baseURL       string
	http          *http.Client
	maxRetries    int
	webhookSecret string
	webhookURL    string

	mu        sync.Mutex
	live      bool // the key's mode, once liveKnown: what payitgo said
	liveKnown bool
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets payitgo's API address. Without it the client uses the
// PAYITGO_BASE_URL environment variable, then "https://api.payitgo.co".
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient replaces the HTTP client (default: 30 s timeout).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithMaxRetries sets how often a request is retried after a network error,
// a 429 or a 5xx (default 2).
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// WithWebhookSecret sets the signing secret of the key's mode, which
// WebhooksService.ConstructEvent uses when given none.
func WithWebhookSecret(secret string) Option { return func(c *Client) { c.webhookSecret = secret } }

// WithWebhookURL sets the webhook_url sent with every payment this client
// creates whose PaymentParams.WebhookURL is empty.
func WithWebhookURL(u string) Option { return func(c *Client) { c.webhookURL = u } }

// NewClient returns a client for apiKey, a test or a live secret key.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("payitgo: the API key is required")
	}
	c := &Client{
		apiKey:     apiKey,
		baseURL:    os.Getenv("PAYITGO_BASE_URL"),
		http:       &http.Client{Timeout: 30 * time.Second},
		maxRetries: 2,
	}
	if c.baseURL == "" {
		c.baseURL = defaultBaseURL
	}
	for _, o := range opts {
		o(c)
	}
	if c.baseURL == "" {
		return nil, errors.New("payitgo: WithBaseURL is empty: leave it out to use https://api.payitgo.co")
	}
	c.baseURL = strings.TrimRight(c.baseURL, "/")
	c.Payments = &PaymentsService{c}
	c.Tokens = &TokensService{c}
	c.Checkout = &CheckoutService{c}
	c.Webhooks = &WebhooksService{c}
	c.Payouts = &PayoutsService{c}
	c.PayoutBatches = &PayoutBatchesService{c}
	c.PayoutWallets = &PayoutWalletsService{c}
	c.Events = &EventsService{c}
	return c, nil
}

// IsLive reports whether the key is a live key (its payments are real) rather
// than a test key. payitgo says which (GET /v1/webhook's livemode) the first
// time; the answer is kept for the client's life. An error (an *Error when
// payitgo refuses the key) is not kept: the next call asks again.
func (c *Client) IsLive(ctx context.Context) (bool, error) {
	c.mu.Lock()
	live, known := c.live, c.liveKnown
	c.mu.Unlock()
	if known {
		return live, nil
	}
	w, err := c.Webhooks.Get(ctx)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	c.live, c.liveKnown = w.Livemode, true
	c.mu.Unlock()
	return w.Livemode, nil
}

// PaymentsService creates and reads payments.
type PaymentsService struct{ c *Client }

// Create creates a payment. Safe to retry: an order with an open payment
// returns that payment.
func (s *PaymentsService) Create(ctx context.Context, p *PaymentParams) (*Payment, error) {
	if p != nil && p.WebhookURL == "" && s.c.webhookURL != "" {
		withURL := *p
		withURL.WebhookURL = s.c.webhookURL
		p = &withURL
	}
	var out Payment
	return &out, s.c.do(ctx, http.MethodPost, "/v1/payments", p, &out, true)
}

// Get returns a payment by its id or by your order id (the order's
// open payment).
func (s *PaymentsService) Get(ctx context.Context, id string) (*Payment, error) {
	var out Payment
	return &out, s.c.do(ctx, http.MethodGet, "/v1/payments/"+url.PathEscape(id), nil, &out, true)
}

// SelectToken sets the coin of a payment created without one, when the
// customer chose it on your own page: one of its TokenOptions. id is the
// payment's id or your order id. The payment comes back whole, now
// with its address, amount, locked rate and a deadline 15 minutes away.
// Another coin changes it while the payment still has TokenOptions; once it
// can no longer change, an *Error with Status 409.
func (s *PaymentsService) SelectToken(ctx context.Context, id, token string) (*Payment, error) {
	var out Payment
	body := map[string]string{"token": token}
	return &out, s.c.do(ctx, http.MethodPost, "/v1/payments/"+url.PathEscape(id)+"/token", body, &out, true)
}

// Simulate plays outcome (an Outcome… constant) on a test payment: payitgo
// sends its deposits, status changes and webhooks a few seconds apart, with
// no coins. id is the payment's id or your order id. The payment
// comes back whole, with its Simulation. A live payment is an *Error with
// Status 400; a simulation that is still running, 409.
func (s *PaymentsService) Simulate(ctx context.Context, id, outcome string) (*Payment, error) {
	var out Payment
	body := map[string]string{"outcome": outcome}
	return &out, s.c.do(ctx, http.MethodPost, "/v1/payments/"+url.PathEscape(id)+"/simulate", body, &out, true)
}

// List returns payments of the key's mode, newest first.
func (s *PaymentsService) List(ctx context.Context, p *ListParams) (*PaymentList, error) {
	q := url.Values{}
	if p != nil {
		for k, v := range map[string]string{"status": p.Status, "chain": p.Chain, "token": p.Token, "store_id": p.StoreID} {
			if v != "" {
				q.Set(k, v)
			}
		}
		if p.Limit > 0 {
			q.Set("limit", strconv.Itoa(p.Limit))
		}
		if p.Offset > 0 {
			q.Set("offset", strconv.Itoa(p.Offset))
		}
	}
	path := "/v1/payments"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out PaymentList
	return &out, s.c.do(ctx, http.MethodGet, path, nil, &out, true)
}

// TokensService lists what customers can pay with.
type TokensService struct{ c *Client }

// TokenListParams narrows a token listing; nil lists every token, in US
// dollars.
type TokenListParams struct {
	// Amount, when set, lists only the tokens a payment of that amount
	// offers (its TokenOptions), each with AmountDue when fees are passed on.
	Amount string
	// Currency is the ISO 4217 code that Amount and every figure listed are
	// in (any case); US dollars when empty.
	Currency string
}

// List returns the tokens of the key's mode, their figures in the currency
// asked for.
func (s *TokensService) List(ctx context.Context, p *TokenListParams) ([]TokenInfo, error) {
	var out struct {
		Currency string      `json:"currency"`
		Tokens   []TokenInfo `json:"tokens"`
	}
	path := "/v1/tokens"
	if p != nil {
		q := url.Values{}
		if p.Amount != "" {
			q.Set("amount", p.Amount)
		}
		if p.Currency != "" {
			q.Set("currency", p.Currency)
		}
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
	}
	err := s.c.do(ctx, http.MethodGet, path, nil, &out, true)
	return out.Tokens, err
}

// CheckoutService reads the payer's view of a payment.
type CheckoutService struct{ c *Client }

// Get returns a payment by its checkout token. Public: sent without the API key.
func (s *CheckoutService) Get(ctx context.Context, checkoutToken string) (*Payment, error) {
	var out Payment
	return &out, s.c.do(ctx, http.MethodGet, "/checkout/"+url.PathEscape(checkoutToken), nil, &out, false)
}

// SelectToken is the customer's choice of coin for a payment created without
// one. Public: sent without the API key.
func (s *CheckoutService) SelectToken(ctx context.Context, checkoutToken, token string) (*Payment, error) {
	var out Payment
	body := map[string]string{"token": token}
	return &out, s.c.do(ctx, http.MethodPost, "/checkout/"+url.PathEscape(checkoutToken)+"/token", body, &out, false)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any, authenticated bool) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "payitgo-go/"+Version)
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if authenticated {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if attempt < c.maxRetries && ctx.Err() == nil {
				if err := sleep(ctx, attempt); err != nil {
					return err
				}
				continue
			}
			return &Error{Message: "request failed: " + err.Error()}
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return &Error{Message: "reading the response: " + err.Error()}
		}
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < c.maxRetries {
			if err := sleep(ctx, attempt); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(data, &e) != nil || e.Error == "" {
				e.Error = http.StatusText(resp.StatusCode)
			}
			return &Error{Status: resp.StatusCode, Message: e.Error}
		}
		return json.Unmarshal(data, out)
	}
}

func sleep(ctx context.Context, attempt int) error {
	t := time.NewTimer(500 * time.Millisecond << attempt)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
