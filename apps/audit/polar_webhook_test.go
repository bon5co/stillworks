package audit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bon5co/godjango/database"
)

var webhookNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// A secret in the shape Polar issues: whsec_ + base64 of the key.
var webhookSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func signedHeaders(t *testing.T, secret, id string, at time.Time, body []byte) http.Header {
	t.Helper()
	keys := webhookKeys(secret)
	stamp := fmt.Sprint(at.Unix())
	header := http.Header{}
	header.Set("webhook-id", id)
	header.Set("webhook-timestamp", stamp)
	header.Set("webhook-signature", signWebhook(keys[0], id, stamp, body))
	return header
}

func TestWebhookSignatureAcceptsOnlyAValidFreshSignature(t *testing.T) {
	body := []byte(`{"type":"subscription.active"}`)
	good := signedHeaders(t, webhookSecret, "msg_1", webhookNow, body)
	verify := func(h http.Header, secret string, payload []byte, at time.Time) error {
		return verifyWebhook(secret, h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature"), payload, at)
	}
	if err := verify(good, webhookSecret, body, webhookNow); err != nil {
		t.Fatalf("a correctly signed delivery was refused: %v", err)
	}
	cases := map[string]error{
		"body altered": verify(good, webhookSecret, []byte(`{"type":"subscription.revoked"}`), webhookNow),
		"wrong secret": verify(good, "whsec_"+base64.StdEncoding.EncodeToString([]byte("another-secret-another-secret-00")), body, webhookNow),
		"stale":        verify(good, webhookSecret, body, webhookNow.Add(webhookTolerance+time.Second)),
		"from future":  verify(good, webhookSecret, body, webhookNow.Add(-webhookTolerance-time.Second)),
		"no secret":    verify(good, "", body, webhookNow),
	}
	missing := http.Header{}
	cases["no headers"] = verify(missing, webhookSecret, body, webhookNow)
	swapped := good.Clone()
	swapped.Set("webhook-id", "msg_other")
	cases["id swapped"] = verify(swapped, webhookSecret, body, webhookNow)
	retimed := good.Clone()
	retimed.Set("webhook-timestamp", fmt.Sprint(webhookNow.Unix()+60))
	cases["timestamp rewritten"] = verify(retimed, webhookSecret, body, webhookNow)
	garbage := good.Clone()
	garbage.Set("webhook-signature", "v1,AAAA")
	cases["garbage signature"] = verify(garbage, webhookSecret, body, webhookNow)
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWebhookSignatureAcceptsAnyEntryInTheList(t *testing.T) {
	body := []byte("{}")
	good := signedHeaders(t, webhookSecret, "msg_2", webhookNow, body)
	list := "v1,AAAA " + good.Get("webhook-signature") + " v2,BBBB"
	if err := verifyWebhook(webhookSecret, "msg_2", good.Get("webhook-timestamp"), list, body, webhookNow); err != nil {
		t.Fatalf("rotation list refused: %v", err)
	}
}

func TestWebhookSignatureHonoursALegacySecret(t *testing.T) {
	// Secrets made before 2026-09-08 sign with the UTF-8 bytes of the whole string.
	legacy := "polar_whs_not_base64_at_all!"
	body := []byte("{}")
	stamp := fmt.Sprint(webhookNow.Unix())
	signature := signWebhook([]byte(legacy), "msg_3", stamp, body)
	if err := verifyWebhook(legacy, "msg_3", stamp, signature, body, webhookNow); err != nil {
		t.Fatalf("legacy secret refused: %v", err)
	}
}

func subscription(id, status string, periodEnd time.Time, fields map[string]any) polarSubscription {
	return polarSubscription{ID: id, Status: status, CurrentPeriodEnd: periodEnd, ProductID: DefaultSponsorProductID, CustomFieldData: fields}
}

func goodFields() map[string]any {
	return map[string]any{
		fieldSponsorProvider: "Acme Inference",
		fieldSponsorPitch:    "Free-tier Llama,\nno card.",
		fieldSponsorURL:      "https://acme.example/signup?ref=stillworks",
	}
}

func TestSlotChangeFollowsTheSubscription(t *testing.T) {
	end := webhookNow.AddDate(0, 1, 0)
	want := map[string]slotChange{
		"active": slotHold, "trialing": slotHold, "canceled": slotEnd, "unpaid": slotEnd,
		"incomplete_expired": slotEnd, "past_due": slotNone, "paused": slotNone, "incomplete": slotNone,
	}
	for status, expected := range want {
		if got := slotChangeFor("subscription.updated", subscription("s", status, end, nil)); got != expected {
			t.Errorf("status %s: got %v want %v", status, got, expected)
		}
	}
	if slotChangeFor("subscription.revoked", subscription("s", "active", end, nil)) != slotEndNow {
		t.Error("a revoked event must end the slot at once, whatever the status says")
	}
}

func TestSponsorFromFieldsUsesTheSameRulesAsTheCommand(t *testing.T) {
	end := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	today := dateOnly(webhookNow)
	sponsor, err := sponsorFromFields(subscription("sub_1", "active", end, goodFields()), today)
	if err != nil {
		t.Fatal(err)
	}
	if sponsor.Pitch != "Free-tier Llama, no card." {
		t.Errorf("line breaks must collapse to one line, got %q", sponsor.Pitch)
	}
	if !sponsor.StartsOn.Equal(today) || !sponsor.EndsOn.Equal(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("dates = %v .. %v, want today .. period end", sponsor.StartsOn, sponsor.EndsOn)
	}
	mutate := func(slug string, value any) map[string]any {
		fields := goodFields()
		if value == nil {
			delete(fields, slug)
		} else {
			fields[slug] = value
		}
		return fields
	}
	bad := map[string]map[string]any{
		"provider missing":  mutate(fieldSponsorProvider, nil),
		"provider blank":    mutate(fieldSponsorProvider, "   "),
		"provider too long": mutate(fieldSponsorProvider, strings.Repeat("a", sponsorProviderMax+1)),
		"pitch too long":    mutate(fieldSponsorPitch, strings.Repeat("a", sponsorPitchMax+1)),
		"url http":          mutate(fieldSponsorURL, "http://acme.example"),
		"url javascript":    mutate(fieldSponsorURL, "javascript:alert(1)"),
		"url no host":       mutate(fieldSponsorURL, "https://"),
		"url relative":      mutate(fieldSponsorURL, "/sponsor"),
		"field not text":    mutate(fieldSponsorURL, 12345),
		"field null":        mutate(fieldSponsorPitch, nil),
	}
	for name, fields := range bad {
		if _, err := sponsorFromFields(subscription("sub_1", "active", end, fields), today); err == nil {
			t.Errorf("%s: published", name)
		}
	}
	if _, err := sponsorFromFields(subscription("sub_1", "active", time.Time{}, goodFields()), today); err == nil {
		t.Error("a subscription with no period end was published")
	}
	if _, err := sponsorFromFields(subscription("sub_1", "active", today.AddDate(0, 0, -2), goodFields()), today); err == nil {
		t.Error("a period already over was published")
	}
}

func event(t *testing.T, kind string, sub polarSubscription) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"id": sub.ID, "status": sub.Status, "current_period_end": sub.CurrentPeriodEnd,
		"product_id": sub.ProductID, "custom_field_data": sub.CustomFieldData,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"type": kind, "timestamp": webhookNow, "data": json.RawMessage(data)})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func post(f *SponsorFulfilment, header http.Header, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, PolarWebhookPath, bytes.NewReader(body))
	for key, values := range header {
		request.Header[key] = values
	}
	recorder := httptest.NewRecorder()
	f.ServeHTTP(recorder, request)
	return recorder
}

func quietFulfilment(db *database.DB, secret string) *SponsorFulfilment {
	f := NewSponsorFulfilment(db, secret, "", slog.New(slog.NewTextHandler(devNull{}, nil)))
	f.now = func() time.Time { return webhookNow }
	return f
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }

func TestWebhookEndpointRefusesTheUnsignedWithoutTouchingTheDatabase(t *testing.T) {
	f := quietFulfilment(nil, webhookSecret)
	body := event(t, "subscription.active", subscription("sub_1", "active", webhookNow.AddDate(0, 1, 0), goodFields()))

	if code := post(f, http.Header{}, body).Code; code != http.StatusUnauthorized {
		t.Errorf("unsigned = %d, want 401", code)
	}
	forged := signedHeaders(t, "whsec_"+base64.StdEncoding.EncodeToString([]byte("not-the-real-secret-not-the-real")), "msg_f", webhookNow, body)
	if code := post(f, forged, body).Code; code != http.StatusUnauthorized {
		t.Errorf("forged = %d, want 401", code)
	}
	stale := signedHeaders(t, webhookSecret, "msg_s", webhookNow.Add(-time.Hour), body)
	if code := post(f, stale, body).Code; code != http.StatusUnauthorized {
		t.Errorf("stale = %d, want 401", code)
	}
	if code := post(quietFulfilment(nil, ""), signedHeaders(t, webhookSecret, "msg_n", webhookNow, body), body).Code; code != http.StatusServiceUnavailable {
		t.Errorf("no configured secret = %d, want 503", code)
	}

	// Signed but not ours: another product in the same organisation, and an
	// event kind that is not about subscriptions. Both are acknowledged so
	// Polar stops retrying, and neither reaches the database (it is nil here).
	other := subscription("sub_o", "active", webhookNow.AddDate(0, 1, 0), goodFields())
	other.ProductID = "00000000-0000-0000-0000-000000000000"
	otherBody := event(t, "subscription.active", other)
	if rec := post(f, signedHeaders(t, webhookSecret, "msg_o", webhookNow, otherBody), otherBody); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("another product = %d %s, want 200 ignored", rec.Code, rec.Body)
	}
	orderBody := []byte(`{"type":"order.paid","data":{}}`)
	if rec := post(f, signedHeaders(t, webhookSecret, "msg_p", webhookNow, orderBody), orderBody); rec.Code != 200 {
		t.Errorf("order event = %d, want 200", rec.Code)
	}
	junk := []byte(`not json`)
	if code := post(f, signedHeaders(t, webhookSecret, "msg_j", webhookNow, junk), junk).Code; code != http.StatusBadRequest {
		t.Errorf("signed junk = %d, want 400", code)
	}
}

// --- database-backed behaviour -------------------------------------------------
//
// These need a real PostgreSQL because the guarantees under test live there:
// the exclusion constraint, the unique subscription id, the advisory lock. Set
// TEST_DATABASE_URL to run them; without it they skip.

func testDB(t *testing.T) *database.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping database-backed fulfilment tests")
	}
	db, err := database.Open(context.Background(), database.DefaultConfig(dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.Bun().ExecContext(ctx, `DROP TABLE IF EXISTS webhook_deliveries, sponsor_orders, sponsors CASCADE`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"20261012100000_create_sponsors.tx.up.sql", "20261012120000_sponsor_fulfilment.tx.up.sql"} {
		sqlText, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Bun().ExecContext(ctx, string(sqlText)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	return db
}

func orderOf(t *testing.T, db *database.DB, id string) sponsorOrder {
	t.Helper()
	var order sponsorOrder
	if err := db.Bun().NewSelect().Model(&order).Where("subscription_id = ?", id).Scan(context.Background()); err != nil {
		t.Fatalf("no order for %s: %v", id, err)
	}
	return order
}

func slotOn(t *testing.T, db *database.DB, day time.Time) *Sponsor {
	t.Helper()
	sponsor, err := ActiveSponsor(context.Background(), db, day)
	if err != nil {
		t.Fatal(err)
	}
	return sponsor
}

func apply(t *testing.T, f *SponsorFulfilment, delivery, kind string, sub polarSubscription) string {
	t.Helper()
	outcome, err := f.Apply(context.Background(), delivery, kind, sub)
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	return outcome
}

func TestPurchaseActivatesRenewsAndCancels(t *testing.T) {
	db := testDB(t)
	f := quietFulfilment(db, webhookSecret)
	first := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	second := time.Date(2026, 12, 12, 10, 0, 0, 0, time.UTC)

	if slotOn(t, db, webhookNow) != nil {
		t.Fatal("slot should start empty")
	}
	if got := apply(t, f, "d1", "subscription.active", subscription("sub_a", "active", first, goodFields())); got != "activated" {
		t.Fatalf("activate = %s", got)
	}
	live := slotOn(t, db, webhookNow)
	if live == nil || live.Provider != "Acme Inference" || live.SubscriptionID != "sub_a" {
		t.Fatalf("slot after purchase = %+v", live)
	}
	if got := live.EndsOn.Format("2006-01-02"); got != "2026-11-12" {
		t.Errorf("ends %s, want the period end 2026-11-12", got)
	}
	if order := orderOf(t, db, "sub_a"); order.Status != orderLive {
		t.Errorf("order = %s", order.Status)
	}

	// Renewal: Polar sends cycled with the next period end. The fields are not
	// resent as new input; the slot's own text stays.
	renewal := subscription("sub_a", "active", second, goodFields())
	if got := apply(t, f, "d2", "subscription.cycled", renewal); got != "extended" {
		t.Fatalf("renewal = %s", got)
	}
	if got := slotOn(t, db, webhookNow).EndsOn.Format("2006-01-02"); got != "2026-12-12" {
		t.Errorf("ends %s after renewal, want 2026-12-12", got)
	}
	// An older event arriving late must not pull the end back in.
	if got := apply(t, f, "d3", "subscription.updated", subscription("sub_a", "active", first, goodFields())); got != "unchanged" {
		t.Errorf("stale update = %s", got)
	}
	if got := slotOn(t, db, webhookNow).EndsOn.Format("2006-01-02"); got != "2026-12-12" {
		t.Errorf("a stale event moved the end to %s", got)
	}

	// Cancelled at period end: the sponsor keeps what was paid for.
	cancel := subscription("sub_a", "active", second, goodFields())
	apply(t, f, "d4", "subscription.canceled", cancel)
	if slotOn(t, db, webhookNow) == nil || slotOn(t, db, second) == nil {
		t.Error("a cancelled subscription lost days it had paid for")
	}
	if slotOn(t, db, second.AddDate(0, 0, 1)) != nil {
		t.Error("the slot outlived the period end")
	}
	// Polar later reports the subscription ended.
	apply(t, f, "d5", "subscription.updated", subscription("sub_a", "canceled", second, goodFields()))
	if slotOn(t, db, second) == nil {
		t.Error("ending after the period must not cut the paid days short")
	}
}

func TestRevocationEndsTheSlotAtOnce(t *testing.T) {
	db := testDB(t)
	f := quietFulfilment(db, webhookSecret)
	end := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	apply(t, f, "r1", "subscription.active", subscription("sub_r", "active", end, goodFields()))
	// Started a day earlier, so revocation shortens rather than deletes it.
	if _, err := db.Bun().ExecContext(context.Background(), `UPDATE sponsors SET starts_on = starts_on - 3`); err != nil {
		t.Fatal(err)
	}
	if got := apply(t, f, "r2", "subscription.revoked", subscription("sub_r", "canceled", end, goodFields())); got != "ended" {
		t.Fatalf("revoke = %s", got)
	}
	if slotOn(t, db, webhookNow) != nil {
		t.Error("a revoked sponsor is still showing")
	}
	if order := orderOf(t, db, "sub_r"); order.Status != orderEnded {
		t.Errorf("order = %s", order.Status)
	}
}

func TestDeliveryIsIdempotent(t *testing.T) {
	db := testDB(t)
	f := quietFulfilment(db, webhookSecret)
	end := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	sub := subscription("sub_i", "active", end, goodFields())

	if got := apply(t, f, "same-id", "subscription.active", sub); got != "activated" {
		t.Fatal(got)
	}
	// Polar retries the same delivery: no second slot, no error.
	if got := apply(t, f, "same-id", "subscription.active", sub); got != "duplicate" {
		t.Errorf("redelivery = %s, want duplicate", got)
	}
	// A different delivery about the same subscription finds the same row.
	if got := apply(t, f, "other-id", "subscription.updated", sub); got != "unchanged" {
		t.Errorf("second event = %s, want unchanged", got)
	}
	sponsors, err := ListSponsors(context.Background(), db)
	if err != nil || len(sponsors) != 1 {
		t.Fatalf("sponsors = %d, err %v; want exactly one", len(sponsors), err)
	}

	// Concurrent first deliveries of one subscription still make one slot.
	concurrent := subscription("sub_c", "active", end, goodFields())
	_ = apply(t, f, "x0", "subscription.revoked", sub) // clear the slot for the next sponsor
	var wait sync.WaitGroup
	for i := 0; i < 6; i++ {
		wait.Add(1)
		go func(n int) {
			defer wait.Done()
			if _, err := f.Apply(context.Background(), fmt.Sprintf("race-%d", n), "subscription.active", concurrent); err != nil {
				t.Errorf("concurrent apply: %v", err)
			}
		}(i)
	}
	wait.Wait()
	var count int
	if err := db.Bun().NewRaw(`SELECT count(*) FROM sponsors WHERE subscription_id = 'sub_c'`).Scan(context.Background(), &count); err != nil || count != 1 {
		t.Errorf("concurrent deliveries made %d slots (err %v), want 1", count, err)
	}
}

func TestInvalidFieldsAreRecordedAndNeverPublished(t *testing.T) {
	db := testDB(t)
	f := quietFulfilment(db, webhookSecret)
	end := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	fields := goodFields()
	fields[fieldSponsorURL] = "javascript:alert(document.cookie)"

	if got := apply(t, f, "bad1", "subscription.active", subscription("sub_bad", "active", end, fields)); got != "rejected" {
		t.Fatalf("outcome = %s, want rejected", got)
	}
	if slotOn(t, db, webhookNow) != nil {
		t.Fatal("a sponsor with an unsafe link was published")
	}
	order := orderOf(t, db, "sub_bad")
	if order.Status != orderRejected || !strings.Contains(order.Reason, "https") || order.URL != "javascript:alert(document.cookie)" {
		t.Errorf("order = %+v; want it kept with the reason", order)
	}
	// A renewal of a rejected order does not publish it either.
	apply(t, f, "bad2", "subscription.cycled", subscription("sub_bad", "active", end.AddDate(0, 1, 0), fields))
	if slotOn(t, db, webhookNow) != nil {
		t.Error("a renewal published a rejected order")
	}
}

func TestAnOccupiedSlotBlocksTheOrderWithoutBreakingAnything(t *testing.T) {
	db := testDB(t)
	f := quietFulfilment(db, webhookSecret)
	if _, err := InsertSponsor(context.Background(), db, Sponsor{
		Provider: "Held by hand", Pitch: "pitch", URL: "https://hand.example",
		StartsOn: dateOnly(webhookNow), EndsOn: dateOnly(webhookNow).AddDate(0, 0, 40),
	}); err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if got := apply(t, f, "blk", "subscription.active", subscription("sub_blk", "active", end, goodFields())); got != "blocked" {
		t.Fatalf("outcome = %s, want blocked", got)
	}
	if slot := slotOn(t, db, webhookNow); slot == nil || slot.Provider != "Held by hand" {
		t.Errorf("the existing sponsor was disturbed: %+v", slot)
	}
	if orderOf(t, db, "sub_blk").Status != orderBlocked {
		t.Error("blocked order was not recorded")
	}
}

func TestSubscriptionNotYetActiveRecordsNothingAndPublishesNothing(t *testing.T) {
	db := testDB(t)
	f := quietFulfilment(db, webhookSecret)
	end := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if got := apply(t, f, "inc", "subscription.created", subscription("sub_n", "incomplete", end, goodFields())); got != "waiting" {
		t.Errorf("outcome = %s", got)
	}
	if slotOn(t, db, webhookNow) != nil {
		t.Error("an unpaid subscription was published")
	}
}

func TestEndToEndOverHTTPWithReplay(t *testing.T) {
	db := testDB(t)
	f := quietFulfilment(db, webhookSecret)
	body := event(t, "subscription.active", subscription("sub_h", "active", time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), goodFields()))
	header := signedHeaders(t, webhookSecret, "msg_h", webhookNow, body)

	first := post(f, header, body)
	if first.Code != 200 || !strings.Contains(first.Body.String(), "activated") {
		t.Fatalf("first = %d %s", first.Code, first.Body)
	}
	// The identical request, replayed byte for byte, authenticates again (the
	// signature is valid) but is recognised and applied zero times more.
	replay := post(f, header, body)
	if replay.Code != 200 || !strings.Contains(replay.Body.String(), "duplicate") {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body)
	}
	if slot := slotOn(t, db, webhookNow); slot == nil || slot.SubscriptionID != "sub_h" {
		t.Errorf("slot = %+v", slot)
	}
	var deliveries int
	if err := db.Bun().NewRaw(`SELECT count(*) FROM webhook_deliveries`).Scan(context.Background(), &deliveries); err != nil || deliveries != 1 {
		t.Errorf("deliveries recorded = %d (err %v), want 1", deliveries, err)
	}
}
