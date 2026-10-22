package payitgo

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ── Mass payouts ──────────────────────────────────────────────────────────────
//
// Paying your own clients (payroll, prizes, withdrawals from your platform)
// from your payout wallet: one address per network, yours alone, that you
// top up. Send each client's payout request as it comes in
// (PayoutsService.Request) and send your open list when you choose
// (PayoutsService.Send), or make whole batches (PayoutBatchesService). A
// payment can be a claim link (its recipient chooses the coin and address)
// or be converted on its way into another coin than the wallet's. Every
// payment is checked and priced before anything leaves the wallet. Reading
// works with any key; making, sending, quoting, submitting, removing,
// canceling and test top-ups need a key with payouts turned on in the
// dashboard (an *Error with Status 403 otherwise).
//
// Every payment ends sent, failed, canceled or expired, with one event
// each (delivered at least once: deduplicate on Event.ID). Events may
// arrive in any order: act on the payment's Status and latest UpdatedAt.
// Replay missed events with EventsService.List, and resync payments with
// PayoutsService.List.

// Payout batch statuses.
const (
	PayoutBatchValidating      = "validating"       // its payments are being checked and priced
	PayoutBatchQuoted          = "quoted"           // checked and priced: submit it while its prices hold (QuoteExpiresAt)
	PayoutBatchPendingApproval = "pending_approval" // submitted; waiting for its approvals
	PayoutBatchAwaitingFunds   = "awaiting_funds"   // approved; waiting for the wallet (Waiting says why)
	PayoutBatchProcessing      = "processing"       // funded: its payments are being sent
	PayoutBatchCompleted       = "completed"        // every payment sent, failed or canceled
	PayoutBatchCanceled        = "canceled"         // canceled before it was funded: nothing was sent
	PayoutBatchRejected        = "rejected"         // an approver refused it (Rejection)
)

// Payout item (a payment to one recipient) statuses.
const (
	PayoutItemValidating = "validating" // being checked
	PayoutItemReady      = "ready"      // checked and priced: it goes
	PayoutItemInvalid    = "invalid"    // Errors say why; never sent, costs nothing
	PayoutItemCooling    = "cooling"    // funded; a new recipient waits until CoolingUntil
	PayoutItemQueued     = "queued"     // funded; waiting for its transfer
	PayoutItemSending    = "sending"    // its transfer is on its way
	PayoutItemSent       = "sent"       // on chain (TxID)
	PayoutItemFailed     = "failed"     // given up (FailReason); its money stays in the wallet
	PayoutItemCanceled   = "canceled"   // canceled, or removed from the open list, before it was sent (CancelReason says why)
	PayoutItemClaimable  = "claimable"  // a claim link, funded: waiting for its recipient (until Claim.ExpiresAt)
	PayoutItemExpired    = "expired"    // a claim link never claimed: its money is free again
)

// Why a payment was canceled (PayoutItem.CancelReason).
const (
	PayoutCancelRemoved       = "removed"        // taken out of your list of payout requests
	PayoutCancelCanceled      = "canceled"       // the payment alone, by your team or an API key
	PayoutCancelBatchCanceled = "batch_canceled" // its batch was canceled
	PayoutCancelRejected      = "rejected"       // its batch was rejected (CancelNote says why)
	PayoutCancelInvalid       = "invalid"        // no longer payable when its batch was submitted or funded (Errors say why)
)

// PayoutCanceledBySystem is PayoutItem.CanceledBy and PayoutBatch.CanceledBy
// when payitgo itself canceled it.
const PayoutCanceledBySystem = "system"

// Who pays a batch's fees (PayoutBatch.FeePaidBy).
const (
	FeePaidByMerchant  = "merchant"  // on top: the recipient receives the whole amount
	FeePaidByRecipient = "recipient" // out of each amount
)

// How a batch pays coins its wallet does not hold (PayoutBatch.Convert).
const (
	PayoutSameNetwork = "same_network" // each payment from the wallet's balance of its own coin
	PayoutConvert     = "convert"      // every payment not in ConvertFrom paid from ConvertFrom, converted on its way
)

// Why a batch waits (PayoutWaiting.Reason).
const (
	PayoutWaitFunds      = "funds"       // the wallet holds less than the batch needs: send the Shortfall
	PayoutWaitDailyLimit = "daily_limit" // the day's limit would be passed: it is funded from 00:00 UTC
	PayoutWaitPaused     = "paused"      // payitgo paused payouts on your account: contact support
)

// Issue codes (PayoutIssue.Code). An error leaves its payment out; a
// warning does not. Codes may be added: treat an unknown one by the list it
// is in (Errors or Warnings) and show its Message.
const (
	IssueUnsupportedAsset      = "unsupported_asset"      // error
	IssueInvalidAmount         = "invalid_amount"         // error
	IssueTooPrecise            = "too_precise"            // error: more decimals than the coin takes
	IssueInvalidAddress        = "invalid_address"        // error
	IssueWrongNetwork          = "wrong_network"          // error: an address of another network
	IssueOwnWallet             = "own_wallet"             // error: your own payout wallet
	IssueDuplicateReference    = "duplicate_reference"    // error: another payment carries the reference
	IssueSanctioned            = "sanctioned"             // error: the address is on the sanctions list
	IssueContractRecipient     = "contract_recipient"     // error: BNB to a contract address
	IssueBelowMinimum          = "below_minimum"          // error: worth less than the smallest payment
	IssueOverLimit             = "over_limit"             // error: over your limit per payment
	IssueFeeExceedsAmount      = "fee_exceeds_amount"     // error: the recipient pays fees that would leave too little
	IssueDuplicatePayment      = "duplicate_payment"      // warning: another row pays the same to the same address
	IssueNewHolder             = "new_holder"             // warning: costlier first USDT to a Tron wallet, in its fee
	IssueActivation            = "activation"             // warning: TRX to a Tron address not activated yet, in its fee
	IssueCheckFailed           = "check_failed"           // warning: priced as the costlier case
	IssueNewRecipient          = "new_recipient"          // warning: an address you never paid waits after funding
	IssueInvalidEmail          = "invalid_email"          // error: a claim link's email is not one
	IssueConversionUnavailable = "conversion_unavailable" // error: paying this coin from the wallet's is not available
)

// PayoutIssue is something wrong with a payment (in Errors: it is left
// out) or worth knowing (in Warnings): Code for programs, Message for
// people, Hint what to do.
type PayoutIssue struct {
	Code    string `json:"code"`            // an Issue… constant
	Field   string `json:"field,omitempty"` // asset, amount, address, tag, reference, email or claim_assets
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// PayoutRecipient is who a payment is for, as you named them.
type PayoutRecipient struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Note  string `json:"note,omitempty"`
}

// PayoutClaim is a claim link's state: a payment its recipient claims,
// choosing the coin (its own, or one of Assets, paid at the same dollar
// value) and the address. URL is the link: emailed to Email when there is
// one (the recipient confirms with a code sent there), yours to share
// otherwise, with PIN, the 6 digits the recipient must enter: give it
// another way than the link (a call, a text message), so the link alone
// pays no one. PINLocked: 5 wrong PINs were entered; a new PIN from the
// dashboard unlocks it. Emailed counts the emails sent, Extended
// how often its end was pushed back. Once claimed in another coin,
// FundedAsset and FundedAmount are what it was funded with (the payment's
// Asset and Amount are then the chosen coin's).
type PayoutClaim struct {
	Email        string     `json:"email,omitempty"`
	Assets       []string   `json:"assets"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	ClaimedAt    *time.Time `json:"claimed_at,omitempty"`
	Emailed      int        `json:"emailed"`
	LastEmailAt  *time.Time `json:"last_email_at,omitempty"`
	Extended     int        `json:"extended"`
	FundedAsset  string     `json:"funded_asset,omitempty"`
	FundedAmount string     `json:"funded_amount,omitempty"`
	URL          string     `json:"url,omitempty"`
	PIN          string     `json:"pin,omitempty"`
	PINLocked    bool       `json:"pin_locked,omitempty"`
}

// PayoutConversion is a payment converted on its way: you asked to pay
// ToAmount of To (its Ticker, on Network); the recipient receives about
// Estimate of it, and AmountOut once it is sent.
type PayoutConversion struct {
	To        string `json:"to"`
	Ticker    string `json:"ticker"`
	Network   string `json:"network"`
	ToAmount  string `json:"to_amount"`
	Estimate  string `json:"estimate"`
	AmountOut string `json:"amount_out,omitempty"`
}

// PayoutItem is one payment to a recipient: a payout request, or a row of a
// batch. Amounts are decimal strings in Asset; while it is validating and
// when it is invalid, its fees and dollar figures are "0". In a quote nothing
// is kept: ID, MerchantID, BatchID and the times are empty. A claim link
// (Claim) has no Address until its recipient claims it. A converted payment
// (Convert) has Asset, Chain, Amount, Fee and Debit in the wallet's coin (the
// batch's ConvertFrom), and Address on Convert.To's network.
type PayoutItem struct {
	ID         string `json:"id,omitempty"`
	MerchantID string `json:"merchant_id,omitempty"`
	Livemode   bool   `json:"livemode"`
	BatchID    string `json:"batch_id,omitempty"` // its batch, or your open list of payout requests; empty for a request removed from the list
	// Reference is your id for it, unique in its mode. RefusedReference is
	// one it was given that another payment carries (it keeps none, and is
	// invalid); ReleasedReference the one it had once canceled or failed
	// (never sent), free for a new payment.
	Reference         string          `json:"reference,omitempty"`
	RefusedReference  string          `json:"refused_reference,omitempty"`
	ReleasedReference string          `json:"released_reference,omitempty"`
	Row               int             `json:"row"` // its place in the batch, from 1
	Recipient         PayoutRecipient `json:"recipient"`
	Asset             string          `json:"asset"`              // the coin paid from the wallet: "USDT_TRC20", …
	Ticker            string          `json:"ticker,omitempty"`   // what people call the coin: "USDT"
	IconURL           string          `json:"icon_url,omitempty"` // its icon, an SVG image
	Chain             string          `json:"chain"`
	Address           string          `json:"address"`
	Tag               *uint32         `json:"tag,omitempty"`
	// Amount is what you asked to pay; SendAmount what reaches the
	// recipient; Fee is PlatformFee and NetworkFee together; Debit what it
	// takes from the wallet. AmountUSD is Amount at USDRate.
	Amount       string        `json:"amount"`
	SendAmount   string        `json:"send_amount"`
	PlatformFee  string        `json:"platform_fee"`
	NetworkFee   string        `json:"network_fee"`
	Fee          string        `json:"fee"`
	Debit        string        `json:"debit"`
	USDRate      string        `json:"usd_rate"`
	AmountUSD    string        `json:"amount_usd"`
	Status       string        `json:"status"` // a PayoutItem… constant
	Errors       []PayoutIssue `json:"errors"`
	Warnings     []PayoutIssue `json:"warnings"`
	CoolingUntil *time.Time    `json:"cooling_until,omitempty"`
	TxID         string        `json:"tx_id,omitempty"`        // once sent (made up in test mode)
	ExplorerURL  string        `json:"explorer_url,omitempty"` // live only
	FailReason   string        `json:"fail_reason,omitempty"`
	CancelReason string        `json:"cancel_reason,omitempty"` // why it was canceled: a PayoutCancel… constant
	CancelNote   string        `json:"cancel_note,omitempty"`   // a rejection's reason (PayoutCancelRejected)
	CanceledBy   string        `json:"canceled_by,omitempty"`   // who canceled it: a team member's id, an API key's id, or PayoutCanceledBySystem
	CanceledAt   *time.Time    `json:"canceled_at,omitempty"`   // when it was canceled
	// Members is the email of the team member CanceledBy names, by id, as it
	// was when they acted.
	Members    map[string]string `json:"members,omitempty"`
	WebhookURL string            `json:"webhook_url,omitempty"` // where its events go instead of its batch's WebhookURL or the webhook's URL
	Claim      *PayoutClaim      `json:"claim,omitempty"`       // a claim link
	// Convert is set on a payment converted on its way; ConversionFee (in
	// Asset, part of Fee) is what converting costs.
	Convert       *PayoutConversion `json:"convert,omitempty"`
	ConversionFee string            `json:"conversion_fee,omitempty"`
	// FeeCredit is the fee given back for a payment never sent, taken off
	// your next batch's fees in its coin.
	FeeCredit string     `json:"fee_credit,omitempty"`
	SentAt    *time.Time `json:"sent_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// PayoutBatch is a set of payments checked, priced, approved and funded
// together. Your open list of payout requests is one too (Inbox and Open):
// it is never submitted itself; sending moves its requests into a batch of
// their own.
type PayoutBatch struct {
	ID         string `json:"id"`
	MerchantID string `json:"merchant_id"`
	Livemode   bool   `json:"livemode"`
	Reference  string `json:"reference,omitempty"` // your id for it, unique in its mode
	Name       string `json:"name"`
	Source     string `json:"source"` // "api", "dashboard", "csv", "requests" or "schedule"
	Inbox      bool   `json:"inbox"`  // made of payout requests
	Open       bool   `json:"open"`   // your open list of payout requests
	// CreatedBy is the team member who made it, or the API key's id (ByAPIKey).
	CreatedBy string `json:"created_by"`
	ByAPIKey  bool   `json:"by_api_key"`
	FeePaidBy string `json:"fee_paid_by"` // FeePaidByMerchant or FeePaidByRecipient
	// Convert is PayoutSameNetwork or PayoutConvert: then every payment not
	// in ConvertFrom is paid from ConvertFrom, converted on its way.
	Convert     string         `json:"convert"`
	ConvertFrom string         `json:"convert_from,omitempty"`
	WebhookURL  string         `json:"webhook_url,omitempty"` // where its events go, and its payments' without their own, instead of the webhook's URL
	Status      string         `json:"status"`                // a PayoutBatch… constant
	Waiting     *PayoutWaiting `json:"waiting,omitempty"`     // while it waits for funds, the day's limit or a pause
	// Totals are its payments that go, per coin; Counts its payments by
	// status; TotalUSD what it takes from the wallet, in dollars.
	Totals          []PayoutAssetTotal `json:"totals"`
	Counts          PayoutCounts       `json:"counts"`
	TotalUSD        string             `json:"total_usd"`
	QuoteExpiresAt  *time.Time         `json:"quote_expires_at,omitempty"` // its prices hold until then
	ApprovalsNeeded int                `json:"approvals_needed"`
	Approvals       []PayoutApproval   `json:"approvals"`
	Rejection       *PayoutDecision    `json:"rejection,omitempty"`
	SubmittedBy     string             `json:"submitted_by,omitempty"`
	SubmittedAt     *time.Time         `json:"submitted_at,omitempty"`
	FundedAt        *time.Time         `json:"funded_at,omitempty"`
	CompletedAt     *time.Time         `json:"completed_at,omitempty"`
	CanceledBy      string             `json:"canceled_by,omitempty"` // a team member's id, an API key's id, or PayoutCanceledBySystem
	CanceledAt      *time.Time         `json:"canceled_at,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// PayoutWaiting is why a batch waits (a PayoutWait… constant), said for
// people in Note; for PayoutWaitFunds, Shortfall is what to send where.
type PayoutWaiting struct {
	Reason    string            `json:"reason"`
	Note      string            `json:"note"`
	Since     time.Time         `json:"since"`
	Shortfall []PayoutShortfall `json:"shortfall,omitempty"`
}

// PayoutShortfall is how much more of Asset the wallet needs, and the
// address (on Network) to send it to.
type PayoutShortfall struct {
	Asset   string `json:"asset"`
	Network string `json:"network"`
	Amount  string `json:"amount"`
	Address string `json:"address"`
}

// PayoutAssetTotal is a batch's payments in one coin: how many (Items), what
// they send, what they pay, and what the wallet gives (Debit, and DebitUSD
// in dollars); SavedUSD is what sending them together saves in network fees.
type PayoutAssetTotal struct {
	Asset       string `json:"asset"`
	Ticker      string `json:"ticker"`
	IconURL     string `json:"icon_url"`
	Chain       string `json:"chain"`
	Network     string `json:"network"`
	Items       int    `json:"items"`
	Amount      string `json:"amount"`
	SendAmount  string `json:"send_amount"`
	PlatformFee string `json:"platform_fee"`
	NetworkFee  string `json:"network_fee"`
	Fee         string `json:"fee"`
	Debit       string `json:"debit"`
	DebitUSD    string `json:"debit_usd"`
	SavedUSD    string `json:"saved_usd"`
}

// PayoutCounts are a batch's payments by status.
type PayoutCounts struct {
	Total     int `json:"total"`
	Invalid   int `json:"invalid"`
	Ready     int `json:"ready"`
	Cooling   int `json:"cooling"`
	Queued    int `json:"queued"`
	Sending   int `json:"sending"`
	Sent      int `json:"sent"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
	Claimable int `json:"claimable"`
	Expired   int `json:"expired"`
}

// PayoutApproval is a team member's approval of a batch.
type PayoutApproval struct {
	UserID string    `json:"user_id"`
	At     time.Time `json:"at"`
}

// PayoutDecision is who refused a batch, when, and why.
type PayoutDecision struct {
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// PayoutWallet is your payout wallet on one network (Chain "tron", "bsc",
// "ethereum", …): the address to top up, and what it holds of each coin it
// pays in. Error says why its balances could not be read just now (they are
// then "0").
type PayoutWallet struct {
	Chain       string              `json:"chain"`
	Network     string              `json:"network"`
	Address     string              `json:"address"`
	ExplorerURL string              `json:"explorer_url,omitempty"` // live only
	Livemode    bool                `json:"livemode"`
	Assets      []PayoutWalletAsset `json:"assets"`
	Error       string              `json:"error,omitempty"`
}

// PayoutWalletAsset is what a payout wallet holds of one coin: Balance (on
// paper in test mode), Reserved for payments, fees and withdrawals not sent
// yet, Spendable the rest, and Needed what batches waiting for funds need
// beyond Spendable: the top-up to send.
type PayoutWalletAsset struct {
	Asset     string `json:"asset"`
	Ticker    string `json:"ticker"`
	IconURL   string `json:"icon_url"`
	Balance   string `json:"balance"`
	Reserved  string `json:"reserved"`
	Spendable string `json:"spendable"`
	Needed    string `json:"needed"`
}

// PayoutWithdrawal is a payout wallet's funds in one coin sent back to one
// of your payout addresses (made in the dashboard, by the owner): Amount
// leaves the wallet, Fee goes to payitgo, SendAmount arrives. Status is
// "requested", "sending", "sent" or "failed" (FailReason says why; its money
// stays in the wallet).
type PayoutWithdrawal struct {
	ID          string     `json:"id"`
	MerchantID  string     `json:"merchant_id"`
	Livemode    bool       `json:"livemode"`
	Asset       string     `json:"asset"`
	Chain       string     `json:"chain"`
	Amount      string     `json:"amount"`
	Fee         string     `json:"fee"`
	SendAmount  string     `json:"send_amount"`
	AddressID   string     `json:"address_id"`
	Address     string     `json:"address"`
	Tag         *uint32    `json:"tag,omitempty"`
	Status      string     `json:"status"`
	TxID        string     `json:"tx_id,omitempty"`
	ExplorerURL string     `json:"explorer_url,omitempty"`
	FailReason  string     `json:"fail_reason,omitempty"` // why it failed
	RequestedBy string     `json:"requested_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
}

// PayoutCoverage is what sending your valid payout requests in one coin
// takes (Needed) against what the wallet can spend; TopUp is the difference
// to send to Address first ("0" when the wallet covers it).
type PayoutCoverage struct {
	Asset     string `json:"asset"`
	Ticker    string `json:"ticker"`
	Network   string `json:"network"`
	Needed    string `json:"needed"`
	Spendable string `json:"spendable"`
	TopUp     string `json:"top_up"`
	Address   string `json:"address"`
}

// PayoutInbox is your open list of payout requests: the list itself (its
// Totals and Counts), what sending its valid requests takes of each coin,
// and one page of the requests.
type PayoutInbox struct {
	Inbox    PayoutBatch      `json:"inbox"`
	Coverage []PayoutCoverage `json:"coverage"`
	Items    []PayoutItem     `json:"items"`
	Total    int              `json:"total"`
	Limit    int              `json:"limit"`
	Offset   int              `json:"offset"`
}

// PayoutQuote is what payments would cost, nothing kept: the totals a batch
// of them would have, and each payment with its fees and issues.
type PayoutQuote struct {
	Quote PayoutQuoteTotals `json:"quote"`
	Items []PayoutItem      `json:"items"`
}

// PayoutQuoteTotals are a quote's totals, as a batch would have them;
// ConvertFrom is empty without conversion.
type PayoutQuoteTotals struct {
	Livemode    bool               `json:"livemode"`
	FeePaidBy   string             `json:"fee_paid_by"`
	Convert     string             `json:"convert"`
	ConvertFrom string             `json:"convert_from"`
	Totals      []PayoutAssetTotal `json:"totals"`
	TotalUSD    string             `json:"total_usd"`
	Counts      PayoutCounts       `json:"counts"`
}

// PayoutBatchList is one page of batches.
type PayoutBatchList struct {
	Batches []PayoutBatch `json:"batches"`
	Total   int           `json:"total"`
	Limit   int           `json:"limit"`
	Offset  int           `json:"offset"`
}

// PayoutItemList is one page of a batch's payments, in row order.
type PayoutItemList struct {
	Items  []PayoutItem `json:"items"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

// PayoutParams is one payment to a recipient: a payout request
// (PayoutsService.Request), or an item of a batch or a quote. Asset and
// Amount are required, and Address unless it is a claim link. Reference,
// your id for the payment, is unique in its mode: send one, so that a retry
// never pays twice. Name, Email and Note are kept with it. Empty optional
// fields are left out.
//
// A claim link has no Address: its recipient chooses the coin (Asset, or
// one of ClaimAssets, at the same dollar value) and the address. Claim
// links are opt-in: Claim, or giving ClaimAssets, makes one; an Email
// without an Address and without Claim is refused like any missing
// address. With an Email its link is emailed there; without one you share
// it (PayoutItem.Claim.URL) yourself, and give its PIN (Claim.PIN) another
// way: the recipient enters it. A claim link is never converted.
//
// WebhookURL (https, on a public host) gets the payment's events, signed
// with the mode's secret: it comes before its batch's WebhookURL and the
// webhook's URL, and a payout request keeps it when it is sent into a batch.
// The payments of one call may name at most 10 different WebhookURLs.
type PayoutParams struct {
	Reference   string   `json:"reference,omitempty"`
	Asset       string   `json:"asset"` // "USDT_TRC20", "BTC", …
	Address     string   `json:"address,omitempty"`
	Tag         *uint32  `json:"tag,omitempty"` // an XRP destination tag
	Amount      string   `json:"amount"`        // decimal, in the coin, e.g. "120"
	Name        string   `json:"name,omitempty"`
	Email       string   `json:"email,omitempty"` // a claim link's link is emailed there
	Note        string   `json:"note,omitempty"`
	Claim       bool     `json:"claim,omitempty"`        // a claim link
	ClaimAssets []string `json:"claim_assets,omitempty"` // the other coins its recipient may choose (a claim link)
	WebhookURL  string   `json:"webhook_url,omitempty"`  // where its events go instead of its batch's or the webhook's URL
}

// PayoutBatchParams makes a batch of 1 to 1000 payments (add more with
// PayoutBatchesService.AddItems, up to 10,000). Reference, your id for the
// batch, makes Create safe to retry. FeePaidBy, Convert and ConvertFrom
// default to your payout settings: with Convert PayoutConvert, every
// payment in another coin than ConvertFrom (a coin, e.g. "USDT_TRC20") is
// paid from ConvertFrom, converted on its way. WebhookURL (https) gets the
// batch's events, and those of its payments without a WebhookURL of their
// own, instead of the webhook's URL.
type PayoutBatchParams struct {
	Reference   string         `json:"reference,omitempty"`
	Name        string         `json:"name,omitempty"`
	FeePaidBy   string         `json:"fee_paid_by,omitempty"`
	Convert     string         `json:"convert,omitempty"`
	ConvertFrom string         `json:"convert_from,omitempty"`
	WebhookURL  string         `json:"webhook_url,omitempty"`
	Items       []PayoutParams `json:"items"`
}

// PayoutQuoteParams prices 1 to 100 payments without keeping anything;
// FeePaidBy, Convert and ConvertFrom as in PayoutBatchParams.
type PayoutQuoteParams struct {
	FeePaidBy   string         `json:"fee_paid_by,omitempty"`
	Convert     string         `json:"convert,omitempty"`
	ConvertFrom string         `json:"convert_from,omitempty"`
	Items       []PayoutParams `json:"items"`
}

// PayoutSchedule is a batch made again and again (set up in the
// dashboard), as payout_schedule.* events tell it: everything but its rows,
// whose recipients stay in the dashboard. Cadence is "weekly" (on Weekday,
// 0 for Sunday), "biweekly" or "monthly" (on Day, 1 to 28, or -1 for the
// month's last day), at At ("09:00") in Timezone. Status is "active" or
// "paused". Each run makes a batch (Source "schedule"): LastBatchID, or
// LastError saying why it made none. NextRunAt and LastRunAt are nil when
// there is none.
type PayoutSchedule struct {
	ID          string                `json:"id"`
	Livemode    bool                  `json:"livemode"`
	Name        string                `json:"name"`
	Status      string                `json:"status"`
	Cadence     string                `json:"cadence"`
	Weekday     int                   `json:"weekday"`
	Day         int                   `json:"day"`
	At          string                `json:"at"`
	Timezone    string                `json:"timezone"`
	FeePaidBy   string                `json:"fee_paid_by"`
	AutoSubmit  bool                  `json:"auto_submit"`
	RemindDays  int                   `json:"remind_days"` // how many days before a run a short wallet is told
	Totals      []PayoutScheduleTotal `json:"totals"`
	NextRunAt   *time.Time            `json:"next_run_at"`
	LastRunAt   *time.Time            `json:"last_run_at"`
	LastBatchID string                `json:"last_batch_id"`
	LastError   string                `json:"last_error"`
	Runs        int                   `json:"runs"`
}

// PayoutScheduleTotal is what a schedule's rows pay in one coin each run
// (fees aside: they are priced at the run).
type PayoutScheduleTotal struct {
	Asset  string `json:"asset"`
	Ticker string `json:"ticker"`
	Items  int    `json:"items"`
	Amount string `json:"amount"`
}

// PayoutScheduleFunding is what a payout_schedule.funding_needed event
// carries: a schedule whose next run the wallet would not cover, and what
// to send where before it.
type PayoutScheduleFunding struct {
	Schedule  PayoutSchedule    `json:"schedule"`
	Shortfall []PayoutShortfall `json:"shortfall"`
}

// PayoutListParams narrows a listing of batches, a batch's payments or
// payout requests (PayoutsService.List takes PayoutPageParams); zero values
// are left out. Status is one status, or for
// PayoutBatchesService.Items and PayoutsService.Inbox several,
// comma-separated ("sent,failed"). Limit is 1 to 200 (default 50).
type PayoutListParams struct {
	Status string
	Limit  int
	Offset int
}

// query is p as a query string: "" or "?…".
func (p *PayoutListParams) query() string {
	if p == nil {
		return ""
	}
	q := url.Values{}
	if p.Status != "" {
		q.Set("status", p.Status)
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Offset > 0 {
		q.Set("offset", strconv.Itoa(p.Offset))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// PayoutPageParams narrows PayoutsService.List; zero values are left out.
type PayoutPageParams struct {
	// Status keeps the payments in these statuses (PayoutItem… constants);
	// none keeps them all.
	Status []string
	// UpdatedSince keeps the payments changed at or after it: for your first
	// read.
	UpdatedSince time.Time
	// Cursor is the NextCursor of the page before: the payments changed after
	// the last one it listed.
	Cursor string
	Limit  int // 1 to 200 (default 50)
}

// PayoutPage is one page of PayoutsService.List: payments, oldest change
// first. While HasMore, read on with Cursor set to NextCursor. NextCursor is
// empty when the page is: keep the cursor you passed.
type PayoutPage struct {
	Items      []PayoutItem `json:"items"`
	HasMore    bool         `json:"has_more"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// PayoutRemoveResult is what PayoutsService.Remove did with one id: the
// request, now canceled (Item), or why it was not removed (Error: Status
// 404 "not found", or 409 for a request sent from the list already or one
// that ended).
type PayoutRemoveResult struct {
	ID    string      `json:"id"`
	Item  *PayoutItem `json:"item,omitempty"`
	Error *Error      `json:"error,omitempty"`
}

type payoutItemReply struct {
	Item PayoutItem `json:"item"`
}

type payoutBatchReply struct {
	Batch PayoutBatch `json:"batch"`
}

type payoutWalletsReply struct {
	Wallets []PayoutWallet `json:"wallets"`
}

// PayoutsService takes your clients' payout requests one by one, sends or
// removes them, and reads, lists, cancels and quotes payments to recipients.
type PayoutsService struct{ c *Client }

// Request adds one of your clients' payout requests to your open list,
// checked and priced at once. A request that fails a check is kept too, with
// Status PayoutItemInvalid and Errors saying why, and is never sent. Safe to
// retry with a Reference: a reference another payment carries returns that
// payment.
func (s *PayoutsService) Request(ctx context.Context, p *PayoutParams) (*PayoutItem, error) {
	var out payoutItemReply
	return &out.Item, s.c.do(ctx, http.MethodPost, "/v1/payouts", p, &out, true)
}

// Retrieve returns a payment to a recipient by its id: a payout request or
// a batch's payment.
func (s *PayoutsService) Retrieve(ctx context.Context, id string) (*PayoutItem, error) {
	var out payoutItemReply
	return &out.Item, s.c.do(ctx, http.MethodGet, "/v1/payouts/"+url.PathEscape(id), nil, &out, true)
}

// RetrieveByReference returns the payment that carries your reference, or
// else the latest payment that gave it back (canceled, failed or expired,
// with it as ReleasedReference): what became of a request sent with it. An
// empty reference is an *Error with Status 400, sent nowhere.
func (s *PayoutsService) RetrieveByReference(ctx context.Context, reference string) (*PayoutItem, error) {
	if strings.TrimSpace(reference) == "" {
		return nil, &Error{Status: http.StatusBadRequest, Message: "reference is required"}
	}
	var out payoutItemReply
	path := "/v1/payouts?" + url.Values{"reference": {reference}}.Encode()
	return &out.Item, s.c.do(ctx, http.MethodGet, path, nil, &out, true)
}

// List returns the payments of the key's mode whatever their batch (payout
// requests in your list, removed ones, and every batch's), oldest change
// first, to bring your records back in sync. Read with UpdatedSince the
// first time, then with Cursor set to NextCursor while HasMore; keep the
// last NextCursor to read what changed since, later. Changes of the last 5
// seconds are left for the next read, so a cursor never skips one.
func (s *PayoutsService) List(ctx context.Context, p *PayoutPageParams) (*PayoutPage, error) {
	q := url.Values{}
	if p != nil {
		if len(p.Status) > 0 {
			q.Set("status", strings.Join(p.Status, ","))
		}
		if !p.UpdatedSince.IsZero() {
			q.Set("updated_since", p.UpdatedSince.UTC().Format(time.RFC3339))
		}
		if p.Cursor != "" {
			q.Set("cursor", p.Cursor)
		}
		if p.Limit > 0 {
			q.Set("limit", strconv.Itoa(p.Limit))
		}
	}
	path := "/v1/payouts"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out PayoutPage
	return &out, s.c.do(ctx, http.MethodGet, path, nil, &out, true)
}

// Inbox returns your open list of payout requests: the list, what sending
// its valid requests needs of each coin against what the wallet can spend
// (Coverage, with the top-up to send first), and a page of the requests.
func (s *PayoutsService) Inbox(ctx context.Context, p *PayoutListParams) (*PayoutInbox, error) {
	var out PayoutInbox
	return &out, s.c.do(ctx, http.MethodGet, "/v1/payouts/inbox"+p.query(), nil, &out, true)
}

// Send sends your open payout requests: those of ids, or every valid one
// when none is given. They move into a new batch, priced again and
// submitted: it then waits for its approvals and funds, or starts. The rest
// stay in the list.
func (s *PayoutsService) Send(ctx context.Context, ids ...string) (*PayoutBatch, error) {
	var out payoutBatchReply
	body := struct {
		IDs []string `json:"ids,omitempty"`
	}{ids}
	return &out.Batch, s.c.do(ctx, http.MethodPost, "/v1/payouts/send", body, &out, true)
}

// Remove takes 1 to 1000 payout requests out of your open list at once.
// Each one still waiting there is canceled (CancelReason
// PayoutCancelRemoved): its reference is free again (ReleasedReference),
// and its record is kept, with no BatchID. It never cancels a request sent
// from the list already: Cancel does. The results come one per id (given
// twice, once), in the order given. A request removed already comes back
// as it is: safe to retry. No ids is an *Error with Status 400, sent
// nowhere.
func (s *PayoutsService) Remove(ctx context.Context, ids ...string) ([]PayoutRemoveResult, error) {
	if len(ids) == 0 {
		return nil, &Error{Status: http.StatusBadRequest, Message: "remove needs at least one payout request id"}
	}
	var out struct {
		Results []PayoutRemoveResult `json:"results"`
	}
	body := struct {
		IDs []string `json:"ids"`
	}{ids}
	err := s.c.do(ctx, http.MethodPost, "/v1/payouts/remove", body, &out, true)
	return out.Results, err
}

// Cancel takes a payout request still in your open list out of it (its
// record is kept: CancelReason PayoutCancelRemoved, no BatchID), or cancels
// a payment that was sent but is not on its way yet (PayoutCancelCanceled).
// It comes back with Status PayoutItemCanceled. A payment canceled already
// comes back as it is: safe to retry.
func (s *PayoutsService) Cancel(ctx context.Context, id string) (*PayoutItem, error) {
	var out payoutItemReply
	return &out.Item, s.c.do(ctx, http.MethodDelete, "/v1/payouts/"+url.PathEscape(id), nil, &out, true)
}

// Quote prices up to 100 payments, checked exactly as in a batch, without
// keeping anything: show a client their fee before they ask for a payout.
func (s *PayoutsService) Quote(ctx context.Context, p *PayoutQuoteParams) (*PayoutQuote, error) {
	var out PayoutQuote
	return &out, s.c.do(ctx, http.MethodPost, "/v1/payout-quotes", p, &out, true)
}

// PayoutBatchesService makes and follows batches: a whole list of payments
// at once.
type PayoutBatchesService struct{ c *Client }

// Create makes a batch; up to 100 payments are checked while the call
// waits and the batch comes back PayoutBatchQuoted (a larger one
// PayoutBatchValidating, then quoted within a minute or two). Safe to retry
// with a Reference: a reference used already returns that batch, with
// nothing added.
func (s *PayoutBatchesService) Create(ctx context.Context, p *PayoutBatchParams) (*PayoutBatch, error) {
	var out payoutBatchReply
	return &out.Batch, s.c.do(ctx, http.MethodPost, "/v1/payout-batches", p, &out, true)
}

// List returns batches of the key's mode, newest first.
func (s *PayoutBatchesService) List(ctx context.Context, p *PayoutListParams) (*PayoutBatchList, error) {
	var out PayoutBatchList
	return &out, s.c.do(ctx, http.MethodGet, "/v1/payout-batches"+p.query(), nil, &out, true)
}

// Retrieve returns a batch by its id.
func (s *PayoutBatchesService) Retrieve(ctx context.Context, id string) (*PayoutBatch, error) {
	var out payoutBatchReply
	return &out.Batch, s.c.do(ctx, http.MethodGet, "/v1/payout-batches/"+url.PathEscape(id), nil, &out, true)
}

// Items returns a page of a batch's payments, in row order.
func (s *PayoutBatchesService) Items(ctx context.Context, id string, p *PayoutListParams) (*PayoutItemList, error) {
	var out PayoutItemList
	return &out, s.c.do(ctx, http.MethodGet, "/v1/payout-batches/"+url.PathEscape(id)+"/items"+p.query(), nil, &out, true)
}

// AddItems adds 1 to 1000 payments to a batch not submitted yet; the whole
// batch is checked and priced again.
func (s *PayoutBatchesService) AddItems(ctx context.Context, id string, items []PayoutParams) (*PayoutBatch, error) {
	body := struct {
		Items []PayoutParams `json:"items"`
	}{items}
	return s.post(ctx, id, "items", body)
}

// Quote checks and prices a batch not submitted yet again, at today's
// prices: they then hold for another hour. Use it when Submit answers 409
// because its prices expired.
func (s *PayoutBatchesService) Quote(ctx context.Context, id string) (*PayoutBatch, error) {
	return s.post(ctx, id, "quote", nil)
}

// Submit sends a quoted batch on: its ready payments go, the invalid ones
// stay out. It waits for its approvals and funds (Waiting says how much to
// send where), or is funded at once.
func (s *PayoutBatchesService) Submit(ctx context.Context, id string) (*PayoutBatch, error) {
	return s.post(ctx, id, "submit", nil)
}

// Cancel cancels a batch not funded yet; once it is processing, its
// payments not sent yet are canceled and their money stays in the wallet.
func (s *PayoutBatchesService) Cancel(ctx context.Context, id string) (*PayoutBatch, error) {
	return s.post(ctx, id, "cancel", nil)
}

func (s *PayoutBatchesService) post(ctx context.Context, id, action string, body any) (*PayoutBatch, error) {
	var out payoutBatchReply
	return &out.Batch, s.c.do(ctx, http.MethodPost, "/v1/payout-batches/"+url.PathEscape(id)+"/"+action, body, &out, true)
}

// PayoutWalletsService reads your payout wallet, and tops up the test one.
type PayoutWalletsService struct{ c *Client }

// List returns your payout wallet of the key's mode, one per network, each
// with what it holds of each coin. It is made the first time it is asked
// for, and never changes.
func (s *PayoutWalletsService) List(ctx context.Context) ([]PayoutWallet, error) {
	var out payoutWalletsReply
	err := s.c.do(ctx, http.MethodGet, "/v1/payout-wallets", nil, &out, true)
	return out.Wallets, err
}

// SimulateTopUp adds amount (a decimal string, over 0 and up to 10,000,000)
// of asset to the test wallet's paper balance, and funds any test batch
// waiting for it. Test keys only: a live key is an *Error with Status 400.
func (s *PayoutWalletsService) SimulateTopUp(ctx context.Context, asset, amount string) ([]PayoutWallet, error) {
	var out payoutWalletsReply
	body := map[string]string{"amount": amount}
	err := s.c.do(ctx, http.MethodPost, "/v1/payout-wallets/"+url.PathEscape(asset)+"/simulate-top-up", body, &out, true)
	return out.Wallets, err
}
