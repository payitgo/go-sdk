# payitgo for Go

Accept crypto payments with [payitgo](https://api.payitgo.co/docs) from Go. Standard library only.

Full reference, with every method, option, type and error: [api.payitgo.co/docs/sdk-go](https://api.payitgo.co/docs/sdk-go).

```bash
go get github.com/thedivinez/payitgo-sdk/go
```

## Accept a payment in three steps

```go
import payitgo "github.com/thedivinez/payitgo-sdk/go"

client, err := payitgo.NewClient(os.Getenv("PAYITGO_KEY"),
	payitgo.WithBaseURL(os.Getenv("PAYITGO_BASE_URL")),
	payitgo.WithWebhookSecret(os.Getenv("PAYITGO_WEBHOOK_SECRET")), // "whsec_…": see Webhooks (IPN) below
)

// 1. Create a payment and send the customer to the checkout page.
func checkout(w http.ResponseWriter, r *http.Request) {
	payment, err := client.Payments.Create(r.Context(), &payitgo.PaymentParams{
		OrderID:    "ORD-1001",   // your order id: safe to retry
		AmountUSD:  "19.99",      // decimal string
		// Token: "USDT_TRC20",   // optional: without it the customer picks the coin
		SuccessURL: "https://shop.example/thanks",
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, payment.CheckoutURL, http.StatusSeeOther)
}

// 2. Receive the notification (the complete handler is under Webhooks (IPN) below).
func ipn(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	event, err := client.Webhooks.ConstructEvent(body, r.Header.Get("X-Webhook-Signature"))
	if err != nil {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	// 3. Fulfil the order once the payment is final.
	if event.Type == payitgo.EventPaymentConfirmed {
		p, _ := event.Payment()
		markOrderPaid(p.OrderID) // make this idempotent: events can arrive twice
	}
	w.WriteHeader(http.StatusOK)
}
```

Start with your test key (`sk_test_…`) and testnet coins; switch to the live key
(`sk_live_…`) when the account owner has turned live mode on.

## Your own payment page

`CheckoutURL` is only for payitgo's hosted page. To show the payment on your own page,
render the payment your server gets back (`DepositAddress`, `CryptoAmount`,
`DestinationTag` for XRP, a QR code of `QRPayload`, `ExpiresAt`); no `Checkout` calls are
needed.

- **You know the coin:** create the payment with `Token` and render the payment it returns.
- **The customer chooses it:** create the payment without `Token`, show
  `payment.TokenOptions` on your page, then call
  `client.Payments.SelectToken(ctx, payment.ID, chosen)` from your server and render the
  payment it returns: it now has its coin, address, amount, locked rate and a deadline 15
  minutes away.

```go
payment, err := client.Payments.Create(ctx, &payitgo.PaymentParams{OrderID: "ORD-1", AmountUSD: "10"})
// your page shows payment.TokenOptions; the customer picks one
chosen, err := client.Payments.SelectToken(ctx, payment.ID, "USDT_TRC20") // or your order id
// render chosen.DepositAddress, chosen.CryptoAmount, chosen.QRPayload, chosen.ExpiresAt
```

The coin is chosen once: choosing again is a `409` (`Payments.Get` then returns the payment
with its coin). A coin outside `TokenOptions`, or a payment that can no longer be paid, is
a `400`.

## Webhooks (IPN)

payitgo POSTs a signed notification for every change of a payment (`payment.confirmed`,
`payment.expired`, …), of a direct settlement and of a payout. Each mode has one webhook:
a test key manages the test one, a live key the live one.

**Where the notifications go.** Set the URL once per mode (https:// on a public host,
accepting POST requests), or give a payment its own when you create it:

```go
// Once per mode (safe to repeat):
_, err := client.Webhooks.Set(ctx, "https://shop.example/payitgo/ipn")

// Or per payment: its events go there instead of the webhook's URL.
_, err = client.Payments.Create(ctx, &payitgo.PaymentParams{
	OrderID: "ORD-1", AmountUSD: "10", WebhookURL: "https://shop.example/orders/ipn",
})

// Or as the client's default for every payment it creates (a payment's own WebhookURL wins):
shop, err := payitgo.NewClient(os.Getenv("PAYITGO_KEY"), payitgo.WithWebhookURL("https://shop.example/orders/ipn"))
```

A storefront's key cannot set the account's URL (`403`): set `WebhookURL` on its payments
instead. Payout events always go to the webhook's URL.

**The signing secret.** Every notification of a mode is signed with that mode's secret
(`whsec_…`), whichever URL it goes to. Copy it from the dashboard or read it with
`client.Webhooks.Get(ctx)` (`.Secret`), and give it to the client once:

```go
client, err := payitgo.NewClient(os.Getenv("PAYITGO_KEY"), payitgo.WithWebhookSecret(os.Getenv("PAYITGO_WEBHOOK_SECRET")))
```

**Receiving them** (`net/http`). Verify the raw body, handle each event once, and answer
`2xx` within 10 seconds; otherwise payitgo retries after 1 min, 5 min, 30 min, 2 h, 6 h and
24 h.

```go
http.HandleFunc("/payitgo/ipn", func(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	// Checks the signature with the client's WithWebhookSecret and refuses anything older than 5 minutes.
	event, err := client.Webhooks.ConstructEvent(body, r.Header.Get("X-Webhook-Signature"))
	if err != nil {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	// The same event can arrive more than once: handle each event id once,
	// e.g. by inserting it into a table with a unique key.
	first, err := recordEventOnce(r.Context(), event.ID)
	if err != nil {
		http.Error(w, "try again", http.StatusInternalServerError) // payitgo retries
		return
	}
	if !first {
		w.WriteHeader(http.StatusOK) // seen before
		return
	}
	switch event.Type {
	case payitgo.EventPaymentConfirmed:
		p, _ := event.Payment()
		jobs.Enqueue("fulfil", p.OrderID) // slow work off the request
	case payitgo.EventPaymentExpired:
		p, _ := event.Payment()
		jobs.Enqueue("release-stock", p.OrderID)
	}
	w.WriteHeader(http.StatusOK)
})
```

**Testing it.** `client.Webhooks.Test(ctx)` queues a signed `ping` event to the webhook's
URL and returns its delivery (`400` when no URL is set).

## API

```go
client, err := payitgo.NewClient(apiKey,
	payitgo.WithBaseURL("https://api.example.com"), // or PAYITGO_BASE_URL
	payitgo.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
	payitgo.WithMaxRetries(2),                      // network errors, 429 and 5xx
	payitgo.WithWebhookSecret("whsec_…"),           // for client.Webhooks.ConstructEvent
	payitgo.WithWebhookURL("https://…"),            // default PaymentParams.WebhookURL
)

client.Livemode                                   // false for sk_test_ keys
client.Payments.Create(ctx, &payitgo.PaymentParams{...}) // OrderID, AmountUSD, Token, …, WebhookURL
client.Payments.SelectToken(ctx, "pay_…", token)  // or your order id: the payment with its coin
client.Payments.Get(ctx, "pay_…")                 // or your order id
client.Payments.List(ctx, &payitgo.ListParams{Status: payitgo.StatusConfirmed, StoreID: "sto_…", Limit: 50})
client.Tokens.List(ctx, nil)                                            // every token
client.Tokens.List(ctx, &payitgo.TokenListParams{AmountUSD: "49.90"})    // the coins a $49.90 payment offers, with AmountDueUSD
client.Checkout.Get(ctx, checkoutToken)           // public payer view
client.Webhooks.Get(ctx)                          // *Webhook{Livemode, URL, Secret, CreatedAt, UpdatedAt}
client.Webhooks.Set(ctx, url)                     // *Webhook
client.Webhooks.Remove(ctx)                       // *Webhook, URL "" (the secret stays)
client.Webhooks.Test(ctx)                         // *WebhookDelivery of a "ping" event
client.Webhooks.ConstructEvent(body, header)      // with WithWebhookSecret
client.Webhooks.ConstructEvent(body, header, secret)
payitgo.ConstructEvent(body, header, secret)        // 5-minute tolerance, no client needed
payitgo.ConstructEventWithTolerance(body, header, secret, tolerance)
```

Amounts are decimal strings (`"19.99"`), never floats.

## Errors

```go
var perr *payitgo.Error
if errors.As(err, &perr) {
	log.Println(perr.Status, perr.Message) // 400 "minimum payment in BTC is $10 right now (network fees)"
}
if errors.Is(err, payitgo.ErrSignature) { /* err says why: signature does not match, timestamp outside the tolerance, … */ }
```

## Development

```bash
go test ./...
```
