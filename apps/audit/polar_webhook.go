package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bon5co/godjango/database"
	"github.com/uptrace/bun"
)

// Self-fulfilling sponsor purchases.
//
// Buying the Polar product "stillworks sponsor slot" posts subscription events
// here, and the slot follows the subscription with nobody running a command:
//
//	subscription active / cycled / updated   slot runs from today to the current period end
//	renewal (a later period end)             the end date moves out; never in
//	canceled (cancel at period end)          nothing to do: the end date already is the period end
//	revoked                                  slot ends at once
//
// Everything is keyed on the Polar subscription id (sponsors.subscription_id),
// so a redelivered or reordered event finds the same row and changes nothing it
// should not. The webhook-id of every processed delivery is stored as well, so
// a retry of a delivery that already succeeded is answered 200 and not applied.
//
// What a buyer typed is untrusted. It goes through Sponsor.Normalised and
// Sponsor.Validate -- the same door `manage sponsor set` uses -- and anything
// that fails is never published: the order is recorded as rejected with the
// reason, the house ad keeps showing, and the log line says a human is needed.

const (
	// PolarWebhookPath is where Polar delivers. It lives on the outermost mux,
	// outside the router, for the reason Analytics.Mount gives: it is a POST from
	// a machine, carries no CSRF token and has its own authentication.
	PolarWebhookPath = "/api/polar/webhook"

	// DefaultSponsorProductID is the Polar product the slot is sold as. The
	// endpoint receives every product in the organisation, so events for other
	// products are acknowledged and ignored.
	DefaultSponsorProductID = "e9bcc586-45ec-4765-92da-873c1ee10448"

	// The custom field slugs on that product's checkout.
	fieldSponsorProvider = "sponsor_provider"
	fieldSponsorPitch    = "sponsor_pitch"
	fieldSponsorURL      = "sponsor_url"

	// polarWebhookMaxBody bounds what is read before the signature is checked.
	polarWebhookMaxBody = 256 << 10

	// Order states, mirrored in the migration's CHECK.
	orderLive     = "live"
	orderRejected = "rejected"
	orderBlocked  = "blocked"
	orderEnded    = "ended"

	// rawFieldMax caps what is stored of a rejected order's text, so a hostile
	// buyer cannot fill the table through a field Polar did not bound.
	rawFieldMax = 2000
)

// polarEvent is the envelope of a delivery.
type polarEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// polarSubscription is the part of a Polar Subscription this uses.
type polarSubscription struct {
	ID                string         `json:"id"`
	Status            string         `json:"status"`
	CurrentPeriodEnd  time.Time      `json:"current_period_end"`
	CancelAtPeriodEnd bool           `json:"cancel_at_period_end"`
	ProductID         string         `json:"product_id"`
	CustomFieldData   map[string]any `json:"custom_field_data"`
}

// SponsorFulfilment turns Polar subscription events into the sponsored slot.
type SponsorFulfilment struct {
	db        *database.DB
	secret    string
	productID string
	now       func() time.Time
	log       *slog.Logger
}

// NewSponsorFulfilment builds the fulfilment. An empty secret makes the
// endpoint refuse everything with 503: unauthenticated input is never accepted
// because configuration is missing. An empty productID means the default.
func NewSponsorFulfilment(db *database.DB, secret, productID string, logger *slog.Logger) *SponsorFulfilment {
	if strings.TrimSpace(productID) == "" {
		productID = DefaultSponsorProductID
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SponsorFulfilment{db: db, secret: strings.TrimSpace(secret), productID: strings.TrimSpace(productID),
		now: time.Now, log: logger}
}

// Mount registers POST /api/polar/webhook. A nil fulfilment mounts nothing.
func (f *SponsorFulfilment) Mount(mux *http.ServeMux) {
	if f == nil {
		return
	}
	mux.HandleFunc("POST "+PolarWebhookPath, f.ServeHTTP)
}

// ServeHTTP authenticates a delivery, then applies it.
//
// Status codes carry meaning for Polar's retry: 2xx means "stop retrying" and
// is returned for everything that retrying cannot improve -- an ignored event,
// a duplicate, an order rejected for its fields. 4xx is an unauthenticated or
// malformed request. 5xx is only ever a failure on our side that a retry can
// fix, which is the one case Polar should try again.
func (f *SponsorFulfilment) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Robots-Tag", "noindex, nofollow")
	if f.secret == "" {
		f.log.Error("stillworks: a Polar webhook arrived but POLAR_WEBHOOK_SECRET is not set; refusing it")
		http.Error(response, "webhook signing secret is not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, polarWebhookMaxBody))
	if err != nil {
		http.Error(response, "unreadable or oversized body", http.StatusBadRequest)
		return
	}
	id := request.Header.Get("webhook-id")
	if err := verifyWebhook(f.secret, id, request.Header.Get("webhook-timestamp"),
		request.Header.Get("webhook-signature"), body, f.now()); err != nil {
		f.log.Warn("stillworks: rejected an unauthenticated Polar webhook", "reason", err.Error())
		http.Error(response, "invalid signature", http.StatusUnauthorized)
		return
	}
	var event polarEvent
	if err := json.Unmarshal(body, &event); err != nil || event.Type == "" {
		http.Error(response, "malformed event", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(event.Type, "subscription.") {
		writeOutcome(response, "ignored")
		return
	}
	var subscription polarSubscription
	if err := json.Unmarshal(event.Data, &subscription); err != nil || subscription.ID == "" {
		http.Error(response, "malformed subscription", http.StatusBadRequest)
		return
	}
	if subscription.ProductID != f.productID {
		writeOutcome(response, "ignored")
		return
	}
	outcome, err := f.Apply(request.Context(), id, event.Type, subscription)
	if err != nil {
		f.log.Error("stillworks: SPONSOR FULFILMENT FAILED, Polar will retry",
			"subscription", subscription.ID, "event", event.Type, "error", err)
		http.Error(response, "could not apply the event", http.StatusInternalServerError)
		return
	}
	writeOutcome(response, outcome)
}

func writeOutcome(response http.ResponseWriter, outcome string) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]string{"outcome": outcome})
}

// slotChange is what an event asks of the slot.
type slotChange int

const (
	slotNone   slotChange = iota // nothing to change
	slotHold                     // the slot should run through the current period end
	slotEnd                      // no renewal: the slot ends at the period end already set
	slotEndNow                   // revoked: the slot ends at once
)

// slotChangeFor decides what an event means from its type and the subscription it
// carries. The subscription's own status is the authority, so an event arriving
// out of order still describes the subscription as Polar holds it.
func slotChangeFor(eventType string, sub polarSubscription) slotChange {
	if eventType == "subscription.revoked" {
		return slotEndNow
	}
	switch sub.Status {
	case "active", "trialing":
		return slotHold
	case "canceled", "incomplete_expired", "unpaid":
		return slotEnd
	}
	// incomplete, past_due, paused: the slot is neither started nor extended. A
	// past_due slot simply lapses at the period end it already has.
	return slotNone
}

// sponsorFromFields builds the sponsor a buyer asked for. The error is the
// reason it cannot be published, in words.
func sponsorFromFields(sub polarSubscription, today time.Time) (Sponsor, error) {
	read := func(slug string) (string, error) {
		raw, present := sub.CustomFieldData[slug]
		if !present || raw == nil {
			return "", fmt.Errorf("checkout field %q is missing", slug)
		}
		text, isString := raw.(string)
		if !isString {
			return "", fmt.Errorf("checkout field %q is not text", slug)
		}
		// A textarea can carry line breaks; the card is one line.
		return strings.Join(strings.Fields(text), " "), nil
	}
	provider, err := read(fieldSponsorProvider)
	if err != nil {
		return Sponsor{}, err
	}
	pitch, err := read(fieldSponsorPitch)
	if err != nil {
		return Sponsor{}, err
	}
	link, err := read(fieldSponsorURL)
	if err != nil {
		return Sponsor{}, err
	}
	if sub.CurrentPeriodEnd.IsZero() {
		return Sponsor{}, errors.New("the subscription carries no current period end")
	}
	ends := dateOnly(sub.CurrentPeriodEnd)
	if ends.Before(today) {
		return Sponsor{}, fmt.Errorf("the subscription's period already ended on %s", ends.Format("2006-01-02"))
	}
	candidate := Sponsor{
		Provider: provider, Pitch: pitch, URL: link,
		StartsOn: today, EndsOn: ends, SubscriptionID: sub.ID,
	}.Normalised()
	if err := candidate.Validate(); err != nil {
		return Sponsor{}, err
	}
	return candidate, nil
}

func clipText(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}

// rawField returns a field as text for the record of an order, whatever its type.
func rawField(sub polarSubscription, slug string) string {
	raw, present := sub.CustomFieldData[slug]
	if !present || raw == nil {
		return ""
	}
	if text, isString := raw.(string); isString {
		return clipText(text, rawFieldMax)
	}
	return clipText(fmt.Sprint(raw), rawFieldMax)
}

// Apply applies one subscription event. It is one transaction: the delivery id,
// the slot and the order are written together or not at all, so a failure
// leaves nothing half done for the retry to trip over. Concurrent events for
// one subscription are serialised by an advisory lock on its id.
func (f *SponsorFulfilment) Apply(ctx context.Context, deliveryID, eventType string, sub polarSubscription) (string, error) {
	outcome := ""
	err := f.db.Bun().RunInTx(ctx, &sql.TxOptions{}, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(?))`, "sponsor-subscription:"+sub.ID); err != nil {
			return err
		}
		if deliveryID != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE received_at < NOW() - INTERVAL '30 days'`); err != nil {
				return err
			}
			inserted, err := tx.ExecContext(ctx, `INSERT INTO webhook_deliveries (id) VALUES (?) ON CONFLICT (id) DO NOTHING`, deliveryID)
			if err != nil {
				return err
			}
			if n, _ := inserted.RowsAffected(); n == 0 {
				outcome = "duplicate"
				return nil
			}
		}
		var err error
		outcome, err = f.applyLocked(ctx, tx, eventType, sub)
		return err
	})
	return outcome, err
}

func (f *SponsorFulfilment) applyLocked(ctx context.Context, tx bun.Tx, eventType string, sub polarSubscription) (string, error) {
	now := f.now()
	today := dateOnly(now)
	todayText := today.Format("2006-01-02")

	var current Sponsor
	found := true
	if err := tx.NewSelect().Model(&current).Where("subscription_id = ?", sub.ID).Scan(ctx); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		found = false
	}
	order := sponsorOrder{
		SubscriptionID: sub.ID, PolarStatus: sub.Status,
		Provider: rawField(sub, fieldSponsorProvider), Pitch: rawField(sub, fieldSponsorPitch), URL: rawField(sub, fieldSponsorURL),
	}
	if !sub.CurrentPeriodEnd.IsZero() {
		end := sub.CurrentPeriodEnd
		order.PeriodEnd = &end
	}
	// The status a later event leaves alone, taken from the stored order.
	var previous sponsorOrder
	hadOrder := true
	if err := tx.NewSelect().Model(&previous).Where("subscription_id = ?", sub.ID).Scan(ctx); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		hadOrder = false
	}

	what := slotChangeFor(eventType, sub)
	outcome := "unchanged"
	switch what {
	case slotNone:
		// Not active yet (or paused, or past due): nothing was published and
		// nothing is changed. A sighting with no order on record is not recorded.
		if !hadOrder {
			return "waiting", nil
		}
		order.Status, order.Reason, order.SponsorID = previous.Status, previous.Reason, previous.SponsorID

	case slotHold:
		periodEnd := dateOnly(sub.CurrentPeriodEnd)
		if found {
			order.SponsorID = &current.ID
			newEnd := dateOnly(current.EndsOn)
			if periodEnd.After(newEnd) {
				newEnd = periodEnd
			}
			if newEnd.Equal(dateOnly(current.EndsOn)) {
				order.Status, order.Reason = orderLive, ""
				if dateOnly(current.EndsOn).Before(today) {
					order.Status = orderEnded
				}
				break
			}
			clash, err := overlaps(ctx, tx, current.ID, current.StartsOn, newEnd)
			if err != nil {
				return "", err
			}
			if clash {
				order.Status = orderLive
				order.Reason = "renewal could not extend the slot: another sponsor holds the following dates"
				f.log.Error("stillworks: SPONSOR RENEWAL NEEDS A HUMAN: another sponsor holds the dates after this one",
					"subscription", sub.ID, "wanted_until", newEnd.Format("2006-01-02"))
				outcome = "blocked"
				break
			}
			if _, err := tx.ExecContext(ctx, `UPDATE sponsors SET ends_on = ?::date WHERE id = ?`,
				newEnd.Format("2006-01-02"), current.ID); err != nil {
				return "", err
			}
			order.Status, order.Reason = orderLive, ""
			outcome = "extended"
			f.log.Info("stillworks: sponsor slot extended", "subscription", sub.ID, "provider", current.Provider,
				"until", newEnd.Format("2006-01-02"))
			break
		}
		// No slot for this subscription yet: this is the purchase.
		sponsor, err := sponsorFromFields(sub, today)
		if err != nil {
			order.Status, order.Reason = orderRejected, err.Error()
			outcome = "rejected"
			f.log.Error("stillworks: SPONSOR ORDER REJECTED, NOTHING PUBLISHED, A HUMAN MUST FOLLOW UP: paid subscription with unusable fields",
				"subscription", sub.ID, "reason", err.Error())
			break
		}
		clash, err := overlaps(ctx, tx, 0, sponsor.StartsOn, sponsor.EndsOn)
		if err != nil {
			return "", err
		}
		if clash {
			order.Status = orderBlocked
			order.Reason = "another sponsor already holds those dates"
			outcome = "blocked"
			f.log.Error("stillworks: SPONSOR ORDER BLOCKED, NOTHING PUBLISHED, A HUMAN MUST FOLLOW UP: another sponsor holds the dates",
				"subscription", sub.ID, "provider", sponsor.Provider)
			break
		}
		if _, err := tx.NewInsert().Model(&sponsor).Exec(ctx); err != nil {
			return "", err
		}
		order.SponsorID = &sponsor.ID
		order.Status, order.Reason = orderLive, ""
		order.Provider, order.Pitch, order.URL = sponsor.Provider, sponsor.Pitch, sponsor.URL
		outcome = "activated"
		f.log.Info("stillworks: sponsor slot ACTIVATED from a Polar purchase", "subscription", sub.ID,
			"provider", sponsor.Provider, "from", todayText, "until", sponsor.EndsOn.Format("2006-01-02"))

	case slotEnd, slotEndNow:
		if !found {
			if hadOrder {
				order.Status, order.Reason, order.SponsorID = previous.Status, previous.Reason, previous.SponsorID
			} else {
				order.Status, order.Reason = orderEnded, "ended before a slot was ever published"
			}
			break
		}
		order.SponsorID = &current.ID
		switch {
		case what == slotEndNow && !current.StartsOn.UTC().Before(today):
			// Never started: nothing was ever shown, so nothing to keep.
			if _, err := tx.ExecContext(ctx, `DELETE FROM sponsors WHERE id = ?`, current.ID); err != nil {
				return "", err
			}
			order.SponsorID = nil
			order.Status, order.Reason = orderEnded, "revoked before the slot started"
			outcome = "ended"
		case what == slotEndNow:
			if dateOnly(current.EndsOn).Before(today) {
				order.Status, order.Reason = orderEnded, "revoked"
				break
			}
			if _, err := tx.ExecContext(ctx, `UPDATE sponsors SET ends_on = ?::date - 1 WHERE id = ?`, todayText, current.ID); err != nil {
				return "", err
			}
			order.Status, order.Reason = orderEnded, "revoked"
			outcome = "ended"
		default:
			// Cancelled: the slot keeps what was paid for. Its end is the period
			// end it already has; only shorten it if the period ended earlier.
			end := dateOnly(current.EndsOn)
			if !sub.CurrentPeriodEnd.IsZero() && dateOnly(sub.CurrentPeriodEnd).Before(end) {
				end = dateOnly(sub.CurrentPeriodEnd)
				if _, err := tx.ExecContext(ctx, `UPDATE sponsors SET ends_on = ?::date WHERE id = ?`,
					end.Format("2006-01-02"), current.ID); err != nil {
					return "", err
				}
				outcome = "ended"
			}
			order.Status = orderLive
			if end.Before(today) {
				order.Status = orderEnded
			}
			order.Reason = "cancelled; the slot ends " + end.Format("2006-01-02")
		}
		f.log.Info("stillworks: sponsor slot ending", "subscription", sub.ID, "event", eventType, "status", order.Status)
	}

	if err := upsertOrder(ctx, tx, order); err != nil {
		return "", err
	}
	return outcome, nil
}

// sponsorOrder is the record kept for every subscription.
type sponsorOrder struct {
	bun.BaseModel `bun:"table:sponsor_orders,alias:so"`

	SubscriptionID string     `bun:"subscription_id,pk"`
	Status         string     `bun:"status,notnull"`
	Reason         string     `bun:"reason,notnull"`
	Provider       string     `bun:"provider,notnull"`
	Pitch          string     `bun:"pitch,notnull"`
	URL            string     `bun:"url,notnull"`
	PolarStatus    string     `bun:"polar_status,notnull"`
	PeriodEnd      *time.Time `bun:"period_end"`
	SponsorID      *int64     `bun:"sponsor_id"`
	CreatedAt      time.Time  `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt      time.Time  `bun:"updated_at,nullzero,notnull,default:now()"`
}

func upsertOrder(ctx context.Context, tx bun.Tx, order sponsorOrder) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO sponsor_orders (subscription_id, status, reason, provider, pitch, url, polar_status, period_end, sponsor_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (subscription_id) DO UPDATE SET
  status = EXCLUDED.status, reason = EXCLUDED.reason,
  provider = EXCLUDED.provider, pitch = EXCLUDED.pitch, url = EXCLUDED.url,
  polar_status = EXCLUDED.polar_status, period_end = EXCLUDED.period_end,
  sponsor_id = EXCLUDED.sponsor_id, updated_at = NOW()`,
		order.SubscriptionID, order.Status, order.Reason, order.Provider, order.Pitch, order.URL,
		order.PolarStatus, order.PeriodEnd, order.SponsorID)
	return err
}

// overlaps reports whether any sponsor other than ignoreID holds a day in the
// range. The database's exclusion constraint is the real guard; asking first
// turns its refusal -- which would abort the whole transaction -- into a
// recorded, readable outcome.
func overlaps(ctx context.Context, tx bun.Tx, ignoreID int64, from, to time.Time) (bool, error) {
	var count int
	err := tx.NewRaw(`SELECT count(*) FROM sponsors
WHERE id <> ? AND daterange(starts_on, ends_on, '[]') && daterange(?::date, ?::date, '[]')`,
		ignoreID, dateOnly(from).Format("2006-01-02"), dateOnly(to).Format("2006-01-02")).Scan(ctx, &count)
	return count > 0, err
}
