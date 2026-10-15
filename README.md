# payitgo for Go

Accept crypto payments with [payitgo](https://api.payitgo.co/docs) from Go. Standard library only.

Full reference, with every method, option, type and error: [api.payitgo.co/docs/sdk-go](https://api.payitgo.co/docs/sdk-go).

```bash
go get github.com/payitgo/go-sdk
```

## Accept a payment in three steps

```go
import payitgo "github.com/payitgo/go-sdk"

client, err := payitgo.NewClient(os.Getenv("PAYITGO_KEY"),
	payitgo.WithBaseURL(os.Getenv("PAYITGO_BASE_URL")),
	payitgo.WithWebhookSecret(os.Getenv("PAYITGO_WEBHOOK_SECRET")), // the signing secret: see Webhooks (IPN) below
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

Start with a test key, where you [simulate](#test-mode) what the customer pays; switch to
a live key when the account owner has turned live mode on. A key does not say its mode
(the dashboard shows a Test or Live badge next to it): `client.IsLive(ctx)` asks payitgo,
once.

## Prices in other currencies

To price a payment in your own currency, give `PriceAmount` and `PriceCurrency` (an ISO 4217
code such as `EUR`, `GBP` or `NGN`) instead of `AmountUSD`. payitgo converts the price into
dollars at the rate of the moment, and the payment keeps both:

```go
payment, err := client.Payments.Create(ctx, &payitgo.PaymentParams{
	OrderID: "ORD-1002", PriceAmount: "45.50", PriceCurrency: "EUR", // decimal string, any case
})
// payment.PriceAmount "45.5", payment.PriceCurrency "EUR",
// payment.PriceRate "1.082" (dollars per euro), payment.USDAmount "49.23"
```

Everything else (`USDAmount`, `CryptoAmount`, fees, minimums) stays in dollars; a payment
priced in dollars has empty `Price…` fields. Give `AmountUSD` or the pair, not both (`400`).
When no exchange rate can be had, `Create` fails with a `503` (`exchange rate unavailable for
price_currency: try again shortly`) once the client's retries are spent: try again shortly.

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

## Test mode

A test payment gets an address like a live one, but no coins are involved: you choose an
outcome and payitgo plays its deposits, status changes and webhooks a few seconds apart
([Test mode](https://api.payitgo.co/docs/testing)).

```go
// Choose the outcome when you create the payment (with Token it starts at once,
// without it once the coin is chosen)…
payment, err := client.Payments.Create(ctx, &payitgo.PaymentParams{
	OrderID: "TEST-1", AmountUSD: "10", Token: "USDT_TRC20", Simulate: payitgo.OutcomeConfirmed,
})

// …or later, on a payment you already have (its id or your order id).
payment, err = client.Payments.Simulate(ctx, "TEST-2", payitgo.OutcomePartiallyPaid)
// payment.Simulation: {Outcome: "partially_paid", State: "running"}
```

Outcomes (`payitgo.Outcome…`): `confirmed`, `overpaid`, `partially_paid`, `underpaid`,
`expired`, `failed`, `wrong_asset`, `paid_late`. Simulated deposits have `Simulated: true`;
their `TxID` names no transaction on any chain. A live key gets `400`; a payment whose
simulation is still running, `409`.

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

**The signing secret.** Every notification of a mode is signed with that mode's secret,
whichever URL it goes to. Copy it from the dashboard or read it with
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
	payitgo.WithWebhookSecret(secret),              // for client.Webhooks.ConstructEvent
	payitgo.WithWebhookURL("https://…"),            // default PaymentParams.WebhookURL
)

client.IsLive(ctx)                                // (false, nil) for a test key: asks payitgo once
client.Payments.Create(ctx, &payitgo.PaymentParams{...}) // OrderID, AmountUSD (or PriceAmount + PriceCurrency), Token, …, WebhookURL, Simulate
client.Payments.SelectToken(ctx, paymentID, token) // or your order id: the payment with its coin
client.Payments.Simulate(ctx, paymentID, payitgo.OutcomeConfirmed) // test keys: plays the outcome
client.Payments.Get(ctx, paymentID)               // or your order id
client.Payments.List(ctx, &payitgo.ListParams{Status: payitgo.StatusConfirmed, StoreID: storeID, Limit: 50})
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
