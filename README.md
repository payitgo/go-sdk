# payitgo for Go

Accept crypto payments, and pay your own clients, with [payitgo](https://api.payitgo.co/docs) from Go.
Standard library only.

Full reference, with every method, option, type and error: [api.payitgo.co/docs/sdk-go](https://api.payitgo.co/docs/sdk-go).

```bash
go get github.com/payitgo/go-sdk
```

## Accept a payment in three steps

```go
import payitgo "github.com/payitgo/go-sdk"

var client *payitgo.Client

func main() {
	var err error
	client, err = payitgo.NewClient(os.Getenv("PAYITGO_KEY"),
		payitgo.WithWebhookSecret(os.Getenv("PAYITGO_WEBHOOK_SECRET")), // the signing secret: see Webhooks (IPN) below
	)
	if err != nil {
		log.Fatal(err)
	}
	http.HandleFunc("/checkout", checkout)
	http.HandleFunc("/payitgo/ipn", ipn)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// 1. Create a payment and send the customer to the checkout page.
func checkout(w http.ResponseWriter, r *http.Request) {
	payment, err := client.Payments.Create(r.Context(), &payitgo.PaymentParams{
		OrderID:    "ORD-1001",   // your order id: safe to retry
		Amount:     "19.99",      // decimal string, in US dollars unless Currency says otherwise
		// Token: "USDT_TRC20",   // optional: without it the customer picks the coin
		SuccessURL: "https://yourdomain.com/thanks",
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

## Prices in your own currency

A payment is priced in US dollars unless you give `Currency`, an ISO 4217 code such as `EUR`,
`GBP` or `NGN` (any case). Every figure on the payment is then in that currency: `Amount` (your
price), `AmountDue` (what the customer pays: the price, plus the fees when you pass them on),
`AmountReceived` (what has arrived so far), each deposit's `Value`, each coin's `Minimum` and
`NetworkFee` in `TokenOptions`, and `RateLocked` (the coin's price). Only `CryptoAmount` and
`CryptoReceived` are in the coin.

```go
payment, err := client.Payments.Create(ctx, &payitgo.PaymentParams{
	OrderID: "ORD-1002", Amount: "45.50", Currency: "EUR", // decimal string; any case
})
// payment.Currency "EUR", payment.Amount "45.5", payment.AmountDue "45.5"
```

payitgo converts at the rate of the moment. When no exchange rate can be had, `Create` fails
with a `503` (`exchange rate unavailable for currency: try again shortly`) once the client's
retries are spent: try again shortly. A payment link priced in a coin has that coin as its
`Currency` (`"ETH"`), and its figures in the coin.

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
payment, err := client.Payments.Create(ctx, &payitgo.PaymentParams{OrderID: "ORD-1", Amount: "10"})
// your page shows payment.TokenOptions; the customer picks one
chosen, err := client.Payments.SelectToken(ctx, payment.ID, "USDT_TRC20") // or your order id
// render chosen.DepositAddress, chosen.CryptoAmount, chosen.QRPayload, chosen.ExpiresAt
```

The customer can change the coin (another of `TokenOptions`) while nothing has arrived, up
to three times; the coin it has already changes nothing. Once the coin can no longer change,
another one is a `409`. A coin outside `TokenOptions`, or a payment that can no longer be paid, is
a `400`.

## Test mode

A test payment gets an address like a live one, but no coins are involved: you choose an
outcome and payitgo plays its deposits, status changes and webhooks a few seconds apart
([Test mode](https://api.payitgo.co/docs/testing)).

```go
// Choose the outcome when you create the payment (with Token it starts at once,
// without it once the coin is chosen)…
payment, err := client.Payments.Create(ctx, &payitgo.PaymentParams{
	OrderID: "TEST-1", Amount: "10", Token: "USDT_TRC20", Simulate: payitgo.OutcomeConfirmed,
})

// …or later, on a payment you already have (its id or your order id).
payment, err = client.Payments.Simulate(ctx, "TEST-2", payitgo.OutcomePartiallyPaid)
// payment.Simulation: {Outcome: "partially_paid", State: "running"}
```

Outcomes (`payitgo.Outcome…`): `confirmed`, `overpaid`, `partially_paid`, `underpaid`,
`expired`, `failed`, `wrong_asset`, `paid_late`, `paid_twice`. Simulated deposits have `Simulated: true`;
their `TxID` names no transaction on any chain. A live key gets `400`; a payment whose
simulation is still running, `409`.

## Webhooks (IPN)

payitgo POSTs a signed notification for every change of a payment (`payment.confirmed`,
`payment.expired`, …), of a direct settlement, of a payout and of your
[mass payouts](#payouts). Each mode has one webhook: a test key manages the test one, a
live key the live one.

**Where the notifications go.** Set the URL once per mode (https:// on a public host,
accepting POST requests), or give a payment its own when you create it:

```go
// Once per mode (safe to repeat):
_, err := client.Webhooks.Set(ctx, "https://api.yourdomain.com/payitgo/ipn")

// Or per payment: its events go there instead of the webhook's URL.
_, err = client.Payments.Create(ctx, &payitgo.PaymentParams{
	OrderID: "ORD-1", Amount: "10", WebhookURL: "https://api.yourdomain.com/orders/ipn",
})

// Or as the client's default for every payment it creates (a payment's own WebhookURL wins):
shop, err := payitgo.NewClient(os.Getenv("PAYITGO_KEY"), payitgo.WithWebhookURL("https://api.yourdomain.com/orders/ipn"))
```

A storefront's key cannot set the account's URL (`403`): set `WebhookURL` on its payments
instead. Payout (`payout.*`) and payout wallet events always go to the webhook's URL. A mass
payout payment's events go to its own `WebhookURL`, else to its batch's, else to the
webhook's URL; a payout batch made with its own `WebhookURL` sends its batch events there.

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

## Follow payments live

Show a payment moving while the customer watches: `client.Realtime.Listen` keeps one
connection to payitgo and hands you every change to your key's payments the moment payitgo
knows it (a deposit seen, each confirmation, the final status), with nothing to poll. It
reconnects by itself and resumes a short drop without missing an update. Fulfil orders
from the webhooks; use this for screens.

```go
go client.Realtime.Listen(ctx, payitgo.Listener{
	Payment: func(u *payitgo.PaymentUpdate) {
		// u.Type: payment.detecting, payment.confirmations, payment.confirmed…
		pushToCheckoutPage(u.Payment.OrderID, u.Payment) // your code: return quickly
	},
	Connected: func(resumed bool) {
		if !resumed {
			refreshOpenOrders() // your code: an update may have been missed while away
		}
	},
	Disconnected: func(err error) { log.Println(err) }, // it connects again by itself
})
```

## Payouts

Pay your own clients (withdrawals from your platform, payroll, prizes) from your **payout
wallet**: one address per network, yours alone, that you top up. payitgo checks and prices
every payment before anything leaves the wallet. Each payment is in one of the payout coins
(`USDT_TRC20`, `USDT_BEP20`, `BTC`, … [the list](https://api.payitgo.co/docs/payouts));
amounts are decimal strings in the coin. Reading works with any key; making, sending,
quoting, removing and canceling need a key with **payouts** turned on in the dashboard
(`403` otherwise). The whole guide: [Mass payouts](https://api.payitgo.co/docs/payouts).

**Payout requests, one by one.** Send each client's payout request as it comes in.
payitgo checks and prices it at once and keeps it in your open list with its costs; send
the list when you choose (or let it go by itself: auto-send, in your payout settings).

```go
// A client asks for a withdrawal. Your reference makes the call safe to retry:
// the same reference again returns this payment (200 instead of 201).
item, err := client.Payouts.Request(ctx, &payitgo.PayoutParams{
	Reference: "WD-7731",
	Asset:     "USDT_TRC20",
	Address:   "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi",
	Amount:    "120",       // decimal string, in the coin
	Name:      "Ana Silva", // optional: Name, Email, Note, and WebhookURL for this payment's events
})
if err != nil {
	return err
}
if item.Status == payitgo.PayoutItemInvalid {
	log.Println(item.Errors[0].Message) // kept, never sent: "this address is for Tron, not BNB Smart Chain"
}
// item.Fee: its whole fee; item.Debit: what it takes from the wallet

// The open list: its requests, and what sending them needs of each coin.
inbox, err := client.Payouts.Inbox(ctx, nil)
for _, c := range inbox.Coverage {
	if c.TopUp != "0" {
		fmt.Printf("send %s %s to %s first\n", c.TopUp, c.Asset, c.Address)
	}
}

// Send every valid request (or some: client.Payouts.Send(ctx, id1, id2)).
batch, err := client.Payouts.Send(ctx)
// batch.Status: pending_approval, awaiting_funds (batch.Waiting says what to send where) or processing;
// validating for more than 100 requests (submitted once priced)

// Take a request out of the list (or cancel it once sent, until it is on its way).
// Its record stays: Status "canceled", CancelReason "removed", ReleasedReference, no BatchID.
// Canceling it again returns it as it is.
_, err = client.Payouts.Cancel(ctx, item.ID)

// Or take many out of the list at once: one result per id, Item or Error (a *payitgo.Error).
results, err := client.Payouts.Remove(ctx, id1, id2)

// Read it later, by its id or by your reference (or the latest payment that gave the
// reference back, once canceled, failed or expired):
item, err = client.Payouts.RetrieveByReference(ctx, "WD-7731") // Status "sent", TxID

// Resync every payment, whatever its batch, oldest change first: keep NextCursor.
page, err := client.Payouts.List(ctx, &payitgo.PayoutPageParams{UpdatedSince: since, Limit: 200})
page, err = client.Payouts.List(ctx, &payitgo.PayoutPageParams{Cursor: page.NextCursor}) // while page.HasMore, and later
```

To show a client the fee before they ask, quote it (up to 100 payments, nothing kept):

```go
q, err := client.Payouts.Quote(ctx, &payitgo.PayoutQuoteParams{
	FeePaidBy: payitgo.FeePaidByRecipient, // the fees come out of the amount
	Items:     []payitgo.PayoutParams{{Asset: "USDT_BEP20", Address: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", Amount: "120"}},
})
// q.Items[0].Fee "0.61183421", q.Items[0].SendAmount "119.38816579", q.Quote.TotalUSD "120"
```

**Batches.** A whole list at once: up to 1000 payments per call, more with `AddItems`, up
to 10,000.

```go
batch, err := client.PayoutBatches.Create(ctx, &payitgo.PayoutBatchParams{
	Reference: "payroll-2026-10", // a reference used already returns that batch: safe to retry
	Name:      "October payroll",
	Items: []payitgo.PayoutParams{
		{Reference: "payroll-2026-10-ana", Asset: "USDT_TRC20", Address: "TR7mNkucnhLZoJQLLJ6DR88jpLQufWxUJi", Amount: "1500"},
		{Reference: "payroll-2026-10-ben", Asset: "USDT_BEP20", Address: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", Amount: "250"},
	},
})
// batch.Status "quoted": Totals per coin, TotalUSD. The rows that failed a check:
invalid, err := client.PayoutBatches.Items(ctx, batch.ID, &payitgo.PayoutListParams{Status: payitgo.PayoutItemInvalid})

batch, err = client.PayoutBatches.Submit(ctx, batch.ID)
if batch.Waiting != nil && batch.Waiting.Reason == payitgo.PayoutWaitFunds {
	for _, s := range batch.Waiting.Shortfall {
		fmt.Printf("top up %s %s at %s\n", s.Amount, s.Asset, s.Address) // it starts by itself once the money arrives
	}
}
```

A batch's prices hold for an hour: past that, `Submit` is a `409` (`its prices expired: get a
new quote first`); call `client.PayoutBatches.Quote(ctx, id)`, then submit. Only a batch not
submitted yet changes (`AddItems`, `Quote`); `Cancel` stops one before it is funded, or the
payments not sent yet of one already processing.

**The wallet.** `client.PayoutWallets.List(ctx)` returns one wallet per network: its
`Address` to top up, and per coin its `Balance`, `Reserved`, `Spendable` and `Needed` (the
top-up that batches waiting for funds need).

**In test mode** a test key has a test payout wallet with balances on paper. Top it up with
a call; batches waiting for it are funded, and their payments are "sent" within seconds
with made-up `TxID`s and the same webhooks as live:

```go
wallets, err := client.PayoutWallets.SimulateTopUp(ctx, "USDT_TRC20", "1000") // test keys only (400 otherwise)
```

**Claim links.** Pay someone whose address you don't have: leave `Address` out and set
`Claim: true` (giving `ClaimAssets` does too). With an `Email`, payitgo emails them the link;
without one, you share it yourself. Claim links are opt-in only: an `Email` without an
`Address` and without `Claim` is refused like any missing address. The recipient opens it, chooses the coin (`Asset`, or one of `ClaimAssets`, paid at the same
dollar value) and their address, and the payment goes. Once its batch is funded it waits,
`claimable`, until `Claim.ExpiresAt`; never claimed, it is `expired` and its money is free
again. Payout requests, batch items and quotes all take them; a claim link is never
converted.

```go
item, err := client.Payouts.Request(ctx, &payitgo.PayoutParams{
	Reference:   "prize-cup-final-2",
	Asset:       "USDT_TRC20",
	Amount:      "100",
	Claim:       true,                          // no Address: its recipient chooses
	Email:       "player2@example.com",         // the link is emailed there
	ClaimAssets: []string{"USDT_BEP20", "BTC"}, // other coins they may choose
})
// item.Claim.URL: the link (share it yourself when there is no email); item.Claim.ExpiresAt once funded
// On payout_item.claimed: item.Asset and item.Address are what they chose, and when that is
// another coin, item.Claim.FundedAsset and FundedAmount are what it was funded with.
```

**Converted payments.** Pay a coin your wallet does not hold: give a batch (or a quote)
`Convert: payitgo.PayoutConvert` and `ConvertFrom`, a coin the wallet holds, and every
payment in another coin is paid from `ConvertFrom`, converted on its way. Without them your
payout settings decide, as they do for payout requests.

```go
batch, err := client.PayoutBatches.Create(ctx, &payitgo.PayoutBatchParams{
	Reference: "payroll-2026-11", Convert: payitgo.PayoutConvert, ConvertFrom: "USDT_TRC20",
	Items: []payitgo.PayoutParams{{Asset: "BTC", Address: "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq", Amount: "0.01"}},
})
// Each converted payment: item.Convert.To "BTC", ToAmount "0.01" (what you asked), Estimate
// (about what arrives), AmountOut once sent. item.Asset, Amount, Fee (converting included)
// and Debit are in USDT_TRC20; item.Address is the recipient's Bitcoin address.
```

A batch with converted payments holds its prices for 15 minutes instead of an hour. A coin
that cannot be paid from `ConvertFrom` is an `invalid` payment (`conversion_unavailable`).

**Webhooks.** Each step raises an event, signed like the others (`event.PayoutBatch()`,
`event.PayoutItem()`, `event.PayoutWithdrawal()`, `event.PayoutSchedule()` and
`event.PayoutScheduleFunding()` decode them):

```go
switch event.Type {
case payitgo.EventPayoutItemSent:
	item, _ := event.PayoutItem()
	markWithdrawalPaid(item.Reference, item.TxID)
case payitgo.EventPayoutItemFailed, payitgo.EventPayoutItemCanceled:
	item, _ := event.PayoutItem() // item.FailReason or item.CancelReason; its money stays in the wallet
	reopenWithdrawal(item.ReleasedReference) // never sent: its reference was given back
case payitgo.EventPayoutBatchAwaitingFunds:
	batch, _ := event.PayoutBatch() // batch.Waiting.Shortfall: what to send where
	alertFinance(batch)
}
```

Batch events (`EventPayoutBatch…`): `payout_batch.quoted`, `.pending_approval`, `.approved`,
`.rejected`, `.awaiting_funds`, `.processing`, `.completed`, `.canceled` (also when payitgo
cancels a batch itself: `CanceledBy` `payitgo.PayoutCanceledBySystem`). Payment events
(`EventPayoutItem…`): `payout_item.processing` (a payout request left your list for a batch)
and `.returned` (it came back, not final), `.sent`, `.failed`, `.canceled` (every canceled
payment, whatever canceled it: `CancelReason`, `CanceledBy`, `CanceledAt`, `CancelNote` for a
rejection), and for claim links `.claimed` and `.claim_expired`. `payout_wallet.withdrawn`
(`EventPayoutWalletWithdrawn`) when a withdrawal you made in the dashboard reaches your
address, `payout_wallet.withdrawal_failed` (`EventPayoutWalletWithdrawalFailed`, with
`FailReason`) when it could not be sent: its money stays in the wallet. Schedules, set up in the
dashboard, send `payout_schedule.run` (`EventPayoutScheduleRun`: the schedule, whose
`LastBatchID` is the batch it made) and `payout_schedule.funding_needed`
(`EventPayoutScheduleFundingNeeded`: the schedule and the `Shortfall` to send before its next
run).

**Staying in sync.** Every payment ends sent, failed, canceled or expired, with one event
each, delivered at least once: deduplicate on `event.ID`, which stays the same however often
it is sent. Order is not guaranteed: act on the payment's `Status` and its latest
`UpdatedAt`. Replay missed events with `client.Events.List` (`After`: the last event ID you
handled), and resync payments with `client.Payouts.List` (keep `NextCursor`):

```go
page, err := client.Events.List(ctx, &payitgo.EventListParams{Types: []string{"payout_item.*"}, After: lastID})
for _, event := range page.Events {
	handleEvent(event) // the same code as your webhook: event.PayoutItem() decodes it; skip IDs seen
}
// while page.HasMore, again with After: the last event's ID
```

## API

```go
client, err := payitgo.NewClient(apiKey,
	payitgo.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
	payitgo.WithMaxRetries(2),                                         // network errors, 429 and 5xx
	payitgo.WithWebhookSecret(secret),                                 // for client.Webhooks.ConstructEvent
	payitgo.WithWebhookURL("https://api.yourdomain.com/payitgo/ipn"), // default PaymentParams.WebhookURL
)

client.IsLive(ctx)                                // (false, nil) for a test key: asks payitgo once
client.Payments.Create(ctx, &payitgo.PaymentParams{...}) // OrderID, Amount, Currency, Token, …, WebhookURL, Simulate
client.Payments.SelectToken(ctx, paymentID, token) // or your order id: the payment with its coin
client.Payments.Simulate(ctx, paymentID, payitgo.OutcomeConfirmed) // test keys: plays the outcome
client.Payments.Get(ctx, paymentID)               // or your order id
client.Payments.List(ctx, &payitgo.ListParams{Status: payitgo.StatusConfirmed, StoreID: storeID, Limit: 50})
client.Tokens.List(ctx, nil)                                            // every token, its figures in US dollars
client.Tokens.List(ctx, &payitgo.TokenListParams{Amount: "49.90", Currency: "EUR"}) // the coins a 49.90 EUR payment offers, with AmountDue
client.Checkout.Get(ctx, checkoutToken)           // public payer view
client.Payouts.Request(ctx, &payitgo.PayoutParams{...})        // Reference, Asset, Address, Amount, Name, …, Claim, ClaimAssets: *PayoutItem
client.Payouts.Retrieve(ctx, itemID)                            // *PayoutItem
client.Payouts.RetrieveByReference(ctx, reference)              // *PayoutItem: the one carrying it, or the latest that gave it back
client.Payouts.List(ctx, &payitgo.PayoutPageParams{Status: []string{"sent", "failed"}, UpdatedSince: since}) // *PayoutPage: Items, HasMore, NextCursor
client.Payouts.Inbox(ctx, &payitgo.PayoutListParams{Status: payitgo.PayoutItemReady}) // *PayoutInbox: Inbox, Coverage, Items, Total
client.Payouts.Send(ctx, ids...)                                // none: every valid request; *PayoutBatch
client.Payouts.Cancel(ctx, itemID)                              // *PayoutItem, canceled (again: as it is)
client.Payouts.Remove(ctx, ids...)                              // 1 to 1000 out of the list: []PayoutRemoveResult (Item or Error)
client.Payouts.Quote(ctx, &payitgo.PayoutQuoteParams{...})      // *PayoutQuote: Quote, Items (nothing kept)
client.PayoutBatches.Create(ctx, &payitgo.PayoutBatchParams{...}) // Reference, Name, FeePaidBy, Convert, ConvertFrom, WebhookURL, Items: *PayoutBatch
client.PayoutBatches.List(ctx, &payitgo.PayoutListParams{Status: payitgo.PayoutBatchCompleted, Limit: 50}) // *PayoutBatchList
client.PayoutBatches.Retrieve(ctx, batchID)                     // *PayoutBatch
client.PayoutBatches.Items(ctx, batchID, &payitgo.PayoutListParams{Status: "sent,failed"}) // *PayoutItemList
client.PayoutBatches.AddItems(ctx, batchID, items)              // *PayoutBatch
client.PayoutBatches.Quote(ctx, batchID)                        // priced again: *PayoutBatch
client.PayoutBatches.Submit(ctx, batchID)                       // *PayoutBatch
client.PayoutBatches.Cancel(ctx, batchID)                       // *PayoutBatch
client.PayoutWallets.List(ctx)                                  // []PayoutWallet
client.PayoutWallets.SimulateTopUp(ctx, "USDT_TRC20", "1000")   // test keys: []PayoutWallet
client.Webhooks.Get(ctx)                          // *Webhook{Livemode, URL, Secret, CreatedAt, UpdatedAt}
client.Webhooks.Set(ctx, url)                     // *Webhook
client.Webhooks.Remove(ctx)                       // *Webhook, URL "" (the secret stays)
client.Webhooks.Test(ctx)                         // *WebhookDelivery of a "ping" event
client.Webhooks.ConstructEvent(body, header)      // with WithWebhookSecret
client.Webhooks.ConstructEvent(body, header, secret)
payitgo.ConstructEvent(body, header, secret)        // 5-minute tolerance, no client needed
payitgo.ConstructEventWithTolerance(body, header, secret, tolerance)
client.Events.List(ctx, &payitgo.EventListParams{Types: []string{"payout_item.*"}, After: lastID}) // *EventPage: Events, HasMore
event.Payment(); event.PayoutBatch(); event.PayoutItem(); event.PayoutWithdrawal() // decode event.Data
event.PayoutSchedule(); event.PayoutScheduleFunding()                             // payout_schedule.* events
client.Realtime.Listen(ctx, payitgo.Listener{Payment: fn, Connected: fn, Disconnected: fn}) // every payment change, live, until ctx ends
```

Amounts are decimal strings (`"19.99"`), never floats.

## Errors

```go
var perr *payitgo.Error
if errors.As(err, &perr) {
	log.Println(perr.Status, perr.Message) // 400 "minimum payment in BTC is $10.00 right now (network fees)"
}
if errors.Is(err, payitgo.ErrSignature) { /* err says why: signature does not match, timestamp outside the tolerance, … */ }
```

## Development

```bash
go test ./...
```
