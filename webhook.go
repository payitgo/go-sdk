package payitgo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance is how old a webhook's timestamp may be.
const DefaultTolerance = 5 * time.Minute

// ErrSignature is returned for a webhook request whose signature or
// timestamp does not check out, wrapped with the reason ("signature does not
// match", …): test for it with errors.Is.
var ErrSignature = errors.New("payitgo: webhook signature verification failed")

// signatureError is ErrSignature with its reason.
func signatureError(reason string) error { return fmt.Errorf("%w: %s", ErrSignature, reason) }

// Event is a webhook event, as its webhook carried it or as EventsService.List
// replays it.
type Event struct {
	// ID identifies the event; it is the same when the event is delivered
	// again, and when it is replayed.
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Livemode  bool            `json:"livemode"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

// Payment decodes the payment a payment.* event carries.
func (e *Event) Payment() (*Payment, error) { return eventData[Payment](e) }

// PayoutBatch decodes the batch a payout_batch.* event carries.
func (e *Event) PayoutBatch() (*PayoutBatch, error) { return eventData[PayoutBatch](e) }

// PayoutItem decodes the payment to a recipient a payout_item.* event
// carries (payout_item.processing, .returned, .sent, .failed, .canceled,
// .claimed and .claim_expired).
func (e *Event) PayoutItem() (*PayoutItem, error) { return eventData[PayoutItem](e) }

// PayoutWithdrawal decodes the withdrawal a payout_wallet.withdrawn or
// payout_wallet.withdrawal_failed event carries.
func (e *Event) PayoutWithdrawal() (*PayoutWithdrawal, error) { return eventData[PayoutWithdrawal](e) }

// PayoutSchedule decodes the schedule a payout_schedule.run event carries.
func (e *Event) PayoutSchedule() (*PayoutSchedule, error) { return eventData[PayoutSchedule](e) }

// PayoutScheduleFunding decodes what a payout_schedule.funding_needed event
// carries: the schedule, and the shortfall to send before its next run.
func (e *Event) PayoutScheduleFunding() (*PayoutScheduleFunding, error) {
	return eventData[PayoutScheduleFunding](e)
}

func eventData[T any](e *Event) (*T, error) {
	var v T
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ConstructEvent verifies a webhook request and returns its event. Pass the
// raw request body (before any JSON parsing), the X-Webhook-Signature header
// and the mode's signing secret (Webhook.Secret, from WebhooksService.Get or
// the dashboard).
//
//	body, _ := io.ReadAll(r.Body)
//	event, err := payitgo.ConstructEvent(body, r.Header.Get("X-Webhook-Signature"), secret)
//	if err != nil {
//		http.Error(w, "bad signature", http.StatusBadRequest)
//		return
//	}
func ConstructEvent(payload []byte, signatureHeader, secret string) (*Event, error) {
	return ConstructEventWithTolerance(payload, signatureHeader, secret, DefaultTolerance)
}

// ConstructEventWithTolerance is ConstructEvent with a custom timestamp
// tolerance; 0 disables the timestamp check. Every verification failure
// wraps ErrSignature with its reason.
func ConstructEventWithTolerance(payload []byte, signatureHeader, secret string, tolerance time.Duration) (*Event, error) {
	var t, v1 string
	for _, part := range strings.Split(signatureHeader, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
			switch k {
			case "t":
				t = v
			case "v1":
				v1 = v
			}
		}
	}
	if t == "" || v1 == "" {
		return nil, signatureError("missing or malformed X-Webhook-Signature header")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "."))
	mac.Write(payload)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(v1)) {
		return nil, signatureError("signature does not match")
	}
	if tolerance > 0 {
		ts, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return nil, signatureError("malformed timestamp")
		}
		if math.Abs(float64(time.Now().Unix()-ts)) > tolerance.Seconds() {
			return nil, signatureError("timestamp outside the tolerance")
		}
	}
	var e Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// ── The webhook (IPN) ─────────────────────────────────────────────────────────

// Event types: every event of the key's mode is sent. EventPing is sent by
// WebhooksService.Test. A payout payment's payout_item.* events go to its own
// WebhookURL, else to its batch's, else to the webhook's URL; a payout batch
// made with a WebhookURL sends its payout_batch.* events there.
const (
	EventPaymentPartiallyPaid = "payment.partially_paid"
	EventPaymentDetecting     = "payment.detecting"
	EventPaymentConfirmed     = "payment.confirmed"
	EventPaymentUnderpaid     = "payment.underpaid"
	EventPaymentExpired       = "payment.expired"
	EventPaymentFailed        = "payment.failed"
	EventPaymentWrongAsset    = "payment.wrong_asset"
	EventPaymentLateDeposit   = "payment.late_deposit"
	EventPayoutApproved       = "payout.approved"
	EventPayoutRejected       = "payout.rejected"
	EventPayoutSent           = "payout.sent"
	EventSettlementCompleted  = "settlement.completed"
	EventSettlementFailed     = "settlement.failed"
	EventPing                 = "ping"
	// Mass payouts: a batch's progress (data: a PayoutBatch), each payment to
	// a recipient (a PayoutItem), the payout wallet's withdrawals (a
	// PayoutWithdrawal), and schedules' runs (a PayoutSchedule) and the
	// top-ups they need (a PayoutScheduleFunding).
	EventPayoutBatchQuoted            = "payout_batch.quoted"
	EventPayoutBatchPendingApproval   = "payout_batch.pending_approval"
	EventPayoutBatchApproved          = "payout_batch.approved"
	EventPayoutBatchRejected          = "payout_batch.rejected"
	EventPayoutBatchAwaitingFunds     = "payout_batch.awaiting_funds"
	EventPayoutBatchProcessing        = "payout_batch.processing"
	EventPayoutBatchCompleted         = "payout_batch.completed"
	EventPayoutBatchCanceled          = "payout_batch.canceled"
	EventPayoutItemProcessing         = "payout_item.processing" // a payout request left your list for a batch of its own
	EventPayoutItemReturned           = "payout_item.returned"   // a sent payout request came back to your list (not final)
	EventPayoutItemSent               = "payout_item.sent"
	EventPayoutItemFailed             = "payout_item.failed"
	EventPayoutItemCanceled           = "payout_item.canceled"
	EventPayoutItemClaimed            = "payout_item.claimed"       // a claim link's recipient chose the coin and address
	EventPayoutItemClaimExpired       = "payout_item.claim_expired" // a claim link was never claimed: its money is free again
	EventPayoutWalletWithdrawn        = "payout_wallet.withdrawn"
	EventPayoutWalletWithdrawalFailed = "payout_wallet.withdrawal_failed" // a withdrawal failed (FailReason): its money stays in the wallet
	EventPayoutScheduleRun            = "payout_schedule.run"             // a schedule made its batch (or says why not)
	EventPayoutScheduleFundingNeeded  = "payout_schedule.funding_needed"  // the wallet would not cover a schedule's next run
)

// Delivery statuses.
const (
	DeliveryPending   = "pending"   // waiting for its next attempt
	DeliverySending   = "sending"   // being posted
	DeliveryDelivered = "delivered" // your server answered 2xx
	DeliveryFailed    = "failed"    // every attempt failed
	DeliveryResolved  = "resolved"  // a failed delivery someone acknowledged
)

// Delivery targets.
const (
	TargetAccount = "account" // the webhook's URL, as it is at each attempt
	TargetPayment = "payment" // the payment's (or payout batch's) own WebhookURL
)

// Webhook is the webhook (IPN) of the key's mode: the URL every event of the
// mode is POSTed to, and the secret that signs them. A test key has the test
// webhook, a live key the live one.
type Webhook struct {
	Livemode bool `json:"livemode"`
	// URL is empty when none is set: events then go only to the payments
	// created with their own WebhookURL.
	URL string `json:"url"`
	// Secret signs every notification of the mode: pass it to
	// WithWebhookSecret.
	Secret    string    `json:"secret"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WebhookDeliveryAttempt is one try at a delivery, or a person's action on it.
type WebhookDeliveryAttempt struct {
	At         time.Time `json:"at"`
	URL        string    `json:"url,omitempty"`    // where it was actually posted
	Signed     string    `json:"signed,omitempty"` // the signature's timestamp
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	Response   string    `json:"response,omitempty"` // the start of the answer your server gave
	Millis     int64     `json:"millis,omitempty"`
	Note       string    `json:"note,omitempty"` // redelivered or resolved, and by whom
}

// WebhookDelivery is one event on its way to one URL.
type WebhookDelivery struct {
	ID         string `json:"id"`
	MerchantID string `json:"merchant_id"`
	Target     string `json:"target"` // TargetAccount or TargetPayment
	URL        string `json:"url"`
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	// Payload is the exact JSON body that is signed and posted.
	Payload        string                   `json:"payload"`
	Status         string                   `json:"status"` // one of the Delivery* constants
	Attempts       int                      `json:"attempts"`
	MaxAttempts    int                      `json:"max_attempts"`
	NextAttemptAt  time.Time                `json:"next_attempt_at"`
	LastStatusCode int                      `json:"last_status_code,omitempty"`
	LastError      string                   `json:"last_error,omitempty"`
	LastAttemptAt  *time.Time               `json:"last_attempt_at,omitempty"`
	DeliveredAt    *time.Time               `json:"delivered_at,omitempty"`
	ResolvedAt     *time.Time               `json:"resolved_at,omitempty"`
	ResolutionNote string                   `json:"resolution_note,omitempty"`
	CreatedAt      time.Time                `json:"created_at"`
	History        []WebhookDeliveryAttempt `json:"history,omitempty"`
}

// WebhooksService manages the webhook (IPN) of the key's mode: a test key the
// test webhook, a live key the live one. A storefront's key may read and test
// it, not change it.
type WebhooksService struct{ c *Client }

type webhookReply struct {
	Webhook Webhook `json:"webhook"`
}

// Get returns the webhook: its URL (empty when not set) and its signing secret.
func (s *WebhooksService) Get(ctx context.Context) (*Webhook, error) {
	var out webhookReply
	return &out.Webhook, s.c.do(ctx, http.MethodGet, "/v1/webhook", nil, &out, true)
}

// Set sets the URL every event of the mode is POSTed to: https, on a public
// host, accepting POST requests. Safe to retry.
func (s *WebhooksService) Set(ctx context.Context, url string) (*Webhook, error) {
	var out webhookReply
	body := map[string]string{"url": url}
	return &out.Webhook, s.c.do(ctx, http.MethodPut, "/v1/webhook", body, &out, true)
}

// Remove stops posting to the URL (payments with their own WebhookURL still
// get theirs); the secret stays.
func (s *WebhooksService) Remove(ctx context.Context) (*Webhook, error) {
	var out webhookReply
	return &out.Webhook, s.c.do(ctx, http.MethodDelete, "/v1/webhook", nil, &out, true)
}

// Test queues a signed "ping" event to the URL and returns its delivery.
func (s *WebhooksService) Test(ctx context.Context) (*WebhookDelivery, error) {
	var out struct {
		Delivery WebhookDelivery `json:"delivery"`
	}
	return &out.Delivery, s.c.do(ctx, http.MethodPost, "/v1/webhook/test", nil, &out, true)
}

// EventListParams narrows EventsService.List; zero values are left out.
type EventListParams struct {
	// Types keeps the events of these types: exact (EventPayoutItemSent), or
	// a family ("payout_item.*"). None keeps them all.
	Types []string
	// After is an event's ID: the events after it. Keep the last one you
	// handled.
	After string
	// CreatedAfter keeps the events created after it (ignored with After).
	CreatedAfter time.Time
	Limit        int // 1 to 200 (default 100)
}

// EventPage is one page of EventsService.List: events, oldest first, each
// decoded with the Event methods as on a webhook. While HasMore, read on with
// After set to the last event's ID.
type EventPage struct {
	Events  []Event `json:"events"`
	HasMore bool    `json:"has_more"`
}

// EventsService replays the events of the key's mode.
type EventsService struct{ c *Client }

// List returns the events of the key's mode exactly as their webhooks
// carried them, oldest first: replay the ones your system missed. Events are
// kept; pings are left out, and events of the last 5 seconds are left for the
// next read, so moving forward never skips one. Any account key reads them
// (read-only keys too); a storefront's key gets an *Error with Status 403. A
// key without payouts gets claim links without their URL and PIN.
func (s *EventsService) List(ctx context.Context, p *EventListParams) (*EventPage, error) {
	q := url.Values{}
	if p != nil {
		if len(p.Types) > 0 {
			q.Set("type", strings.Join(p.Types, ","))
		}
		if p.After != "" {
			q.Set("after", p.After)
		}
		if !p.CreatedAfter.IsZero() {
			q.Set("created_after", p.CreatedAfter.UTC().Format(time.RFC3339))
		}
		if p.Limit > 0 {
			q.Set("limit", strconv.Itoa(p.Limit))
		}
	}
	path := "/v1/events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out EventPage
	return &out, s.c.do(ctx, http.MethodGet, path, nil, &out, true)
}

// ConstructEvent verifies a webhook request with the client's
// WithWebhookSecret, or with secret when one is passed, and returns its event
// (see the package's ConstructEvent). Without any secret it returns an error
// wrapping ErrSignature.
func (s *WebhooksService) ConstructEvent(payload []byte, signatureHeader string, secret ...string) (*Event, error) {
	key := s.c.webhookSecret
	if len(secret) > 0 && secret[0] != "" {
		key = secret[0]
	}
	if key == "" {
		return nil, signatureError("no webhook secret: pass one, or use WithWebhookSecret")
	}
	return ConstructEvent(payload, signatureHeader, key)
}
