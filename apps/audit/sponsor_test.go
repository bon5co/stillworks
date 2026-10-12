package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shelfFixture() []WorkingModel {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	return []WorkingModel{
		{Slug: "alpha", BaseURL: "https://api.alpha.example", ChatPath: "/v1/chat/completions", AuthMode: AuthModeNone,
			ModelID: "alpha-8b", ChatCapable: true, OpenAICompatible: true, LastOK: &now, LatencyMS: 300, Attempts: 10, Successes: 10},
		{Slug: "beta", BaseURL: "https://api.beta.example", ChatPath: "/v1/chat/completions", AuthMode: AuthModeNone,
			ModelID: "beta-70b", ChatCapable: true, OpenAICompatible: true, LastOK: &now, LatencyMS: 900, Attempts: 10, Successes: 8},
		{Slug: "gamma", BaseURL: "https://api.gamma.example", ChatPath: "/v1/chat/completions", AuthMode: AuthModeNone,
			ModelID: "gamma-mini", ChatCapable: true, OpenAICompatible: true, LastOK: &now, LatencyMS: 1500, Attempts: 10, Successes: 5},
	}
}

func sponsorFixture() Sponsor {
	return Sponsor{
		Provider: "Acme Inference", Pitch: "Free-tier Llama and Qwen, no card.",
		URL: "https://acme.example/signup?ref=stillworks", Model: "gamma-mini", BaseURL: "https://api.gamma.example/v1",
		EndpointSlug: "gamma",
		StartsOn:     time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		EndsOn:       time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
	}
}

// The invariant this whole feature is built around. Callers of /api/llm/up take
// models[0] as the pick and models[1:] as the fallback chain; a paid entry that
// could enter or move anything in that array would put the product up for sale.
// The payload with a sponsor must carry the same models, byte for byte, as the
// payload without one -- including when the sponsor's own endpoint is on the
// shelf and ranked last.
func TestSponsoredNeverEntersModelsOrChangesTheirOrder(t *testing.T) {
	shelf := shelfFixture()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	sponsor := sponsorFixture()

	without := upPayload(shelf, AuthModeNone, nil, nil, now)
	probe := ProbeFor(sponsor, shelf)
	entry := NewSponsoredEntry(sponsor, probe, "https://stillworks.example")
	with := upPayload(shelf, AuthModeNone, nil, entry, now)

	modelsOf := func(payload map[string]any) []byte {
		t.Helper()
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded["models"]
	}
	if !bytes.Equal(modelsOf(without), modelsOf(with)) {
		t.Fatalf("a sponsor changed the models array:\nwithout: %s\nwith:    %s", modelsOf(without), modelsOf(with))
	}

	var decoded struct {
		Models []struct {
			Endpoint string `json:"endpoint"`
			Model    string `json:"model"`
		} `json:"models"`
		Count int `json:"count"`
	}
	encoded, _ := json.Marshal(with)
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Count != 3 || len(decoded.Models) != 3 {
		t.Fatalf("count/models = %d/%d, want 3/3", decoded.Count, len(decoded.Models))
	}
	for i, want := range []string{"alpha-8b", "beta-70b", "gamma-mini"} {
		if decoded.Models[i].Model != want {
			t.Fatalf("models[%d] = %q, want %q: the ranking moved", i, decoded.Models[i].Model, want)
		}
	}
	// The sponsor's name is nowhere inside the models array.
	if bytes.Contains(modelsOf(with), []byte("Acme Inference")) || bytes.Contains(modelsOf(with), []byte("sponsored")) {
		t.Fatalf("sponsor text leaked into models: %s", modelsOf(with))
	}
}

func TestSponsoredFieldIsNullWhenEmptyAndTypedWhenPresent(t *testing.T) {
	now := time.Now()
	empty, _ := json.Marshal(upPayload(shelfFixture(), AuthModeNone, nil, nil, now))
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(empty, &shape); err != nil {
		t.Fatal(err)
	}
	raw, present := shape["sponsored"]
	if !present || string(raw) != "null" {
		t.Fatalf("with no sponsor, sponsored = %s (present=%v), want an explicit null", raw, present)
	}

	entry := NewSponsoredEntry(sponsorFixture(), nil, "https://stillworks.example/")
	full, _ := json.Marshal(upPayload(shelfFixture(), AuthModeNone, nil, entry, now))
	var decoded struct {
		Sponsored struct {
			Sponsored       bool            `json:"sponsored"`
			Label           string          `json:"label"`
			Notice          string          `json:"notice"`
			Provider        string          `json:"provider"`
			URL             string          `json:"url"`
			DestinationHost string          `json:"destination_host"`
			Probe           json.RawMessage `json:"probe"`
		} `json:"sponsored"`
	}
	if err := json.Unmarshal(full, &decoded); err != nil {
		t.Fatal(err)
	}
	s := decoded.Sponsored
	if !s.Sponsored || s.Label != "Sponsored" || s.Provider != "Acme Inference" || s.Notice == "" {
		t.Fatalf("sponsored entry not typed as sponsored: %+v", s)
	}
	if s.URL != "https://stillworks.example/go/sponsor?src=api" {
		t.Fatalf("tracked url = %q", s.URL)
	}
	if s.DestinationHost != "acme.example" {
		t.Fatalf("destination_host = %q", s.DestinationHost)
	}
	if string(s.Probe) != "null" {
		t.Fatalf("with no probe the probe field must be null, got %s", s.Probe)
	}
}

// A probe is real data or nothing. A sponsor whose endpoint is not on the shelf,
// or whose named model is not the one we measured, gets no probe -- never a
// synthesised green.
func TestProbeForIsRealDataOrNil(t *testing.T) {
	shelf := shelfFixture()
	sponsor := sponsorFixture()

	got := ProbeFor(sponsor, shelf)
	if got == nil || got.Slug != "gamma" || got.Successes != 5 || got.Attempts != 10 || got.LatencyMS != 1500 {
		t.Fatalf("probe = %+v, want gamma's own row (5/10, 1500 ms)", got)
	}
	got.Successes = 10 // mutating the copy must not touch the shelf
	if shelf[2].Successes != 5 {
		t.Fatal("ProbeFor returned a pointer into the shelf")
	}

	sponsor.EndpointSlug = "not-on-the-shelf"
	if ProbeFor(sponsor, shelf) != nil {
		t.Fatal("a sponsor whose endpoint is not on the shelf got probe data")
	}
	sponsor = sponsorFixture()
	sponsor.Model = "gamma-unmeasured"
	if ProbeFor(sponsor, shelf) != nil {
		t.Fatal("a model we never measured got another model's probe")
	}
	sponsor = sponsorFixture()
	sponsor.EndpointSlug = ""
	if ProbeFor(sponsor, shelf) != nil {
		t.Fatal("a sponsor naming no endpoint got probe data")
	}
}

func TestSponsorValidation(t *testing.T) {
	if err := sponsorFixture().Validate(); err != nil {
		t.Fatalf("fixture refused: %v", err)
	}
	cases := map[string]func(*Sponsor){
		"empty provider":     func(s *Sponsor) { s.Provider = "  " },
		"long provider":      func(s *Sponsor) { s.Provider = strings.Repeat("a", 81) },
		"empty pitch":        func(s *Sponsor) { s.Pitch = "" },
		"long pitch":         func(s *Sponsor) { s.Pitch = strings.Repeat("é", 201) },
		"http url":           func(s *Sponsor) { s.URL = "http://acme.example" },
		"javascript url":     func(s *Sponsor) { s.URL = "javascript:alert(1)" },
		"relative url":       func(s *Sponsor) { s.URL = "/signup" },
		"http base url":      func(s *Sponsor) { s.BaseURL = "http://api.acme.example" },
		"end before start":   func(s *Sponsor) { s.EndsOn = s.StartsOn.AddDate(0, 0, -1) },
		"missing dates":      func(s *Sponsor) { s.StartsOn = time.Time{} },
		"url without a host": func(s *Sponsor) { s.URL = "https://" },
	}
	for name, mutate := range cases {
		s := sponsorFixture()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSponsorActiveWindowIsInclusiveUTC(t *testing.T) {
	s := sponsorFixture()
	for day, want := range map[string]bool{
		"2026-09-30": false, "2026-10-01": true, "2026-10-31": true, "2026-11-01": false,
	} {
		parsed, _ := time.Parse("2006-01-02", day)
		if got := s.Active(parsed.Add(23 * time.Hour)); got != want {
			t.Errorf("Active(%s) = %v, want %v", day, got, want)
		}
	}
}

// The click is filed under the place it came from, in the existing traffic
// pipeline, and the source is one of two known strings.
func TestRecordAsFilesTheSponsorClickUnderItsSource(t *testing.T) {
	recorder := &Recorder{events: make(chan TrafficEvent, 4), salt: "test-salt", now: time.Now}
	request := httptest.NewRequest(http.MethodGet, "/go/sponsor?src=api", nil)
	request.Header.Set("User-Agent", "python-requests/2.32")
	recorder.RecordAs(request, KindSponsor, sponsorClickPath+"?src="+clickSourceAPI, http.StatusFound)

	crawler := httptest.NewRequest(http.MethodGet, "/go/sponsor", nil)
	crawler.Header.Set("User-Agent", "Googlebot/2.1")
	recorder.RecordAs(crawler, KindSponsor, sponsorClickPath+"?src="+clickSourcePage, http.StatusFound)

	first, second := <-recorder.events, <-recorder.events
	if first.Kind != KindSponsor || first.Path != "/go/sponsor?src=api" || first.Status != http.StatusFound || first.IsCrawler {
		t.Fatalf("api click recorded as %+v", first)
	}
	if !second.IsCrawler {
		t.Fatal("a crawler following the link was not classified as one, so it would be counted as a click")
	}

	var nilRecorder *Recorder
	nilRecorder.RecordAs(request, KindSponsor, "x", 302) // must not panic: a management process has none
}

func TestSponsorCardLabelsPaidPlacementAndNeverForgesAProbe(t *testing.T) {
	sponsor := sponsorFixture()
	shelf := shelfFixture()

	withProbe := renderPage(t, sponsorCard(SponsorSlot{Sponsor: &sponsor, Probe: ProbeFor(sponsor, shelf)}))
	for _, want := range []string{
		`>Sponsored<`, `rel="sponsored noopener"`, `href="/go/sponsor?src=page"`, "Acme Inference",
		"acme.example", "answered 5/10", "1500 ms",
	} {
		if !strings.Contains(withProbe, want) {
			t.Errorf("sponsor card missing %q:\n%s", want, withProbe)
		}
	}
	if strings.Contains(withProbe, "ref=stillworks") {
		t.Error("the card links the destination directly, so the click would not be counted")
	}
	if strings.Contains(withProbe, "<tr") {
		t.Error("the sponsor is rendered as a table row")
	}

	noProbe := renderPage(t, sponsorCard(SponsorSlot{Sponsor: &sponsor}))
	if !strings.Contains(noProbe, "Not a verified row") {
		t.Errorf("a sponsor with no probe data should say so:\n%s", noProbe)
	}
	for _, forbidden := range []string{"answered ", " ms"} {
		if strings.Contains(noProbe, forbidden) {
			t.Errorf("sponsor with no probe shows %q, which would be invented status", forbidden)
		}
	}
}

func TestHouseAdShowsWhenNoSponsorAndHomePageStillHoldsTheShelf(t *testing.T) {
	body := renderPage(t, HomePage(nil, 0, ParseShelfQuery(nil), nil, "https://stillworks.supercapybara.com"))
	for _, want := range []string{"Sponsor this slot", `href="/sponsor"`, "Open slot"} {
		if !strings.Contains(body, want) {
			t.Errorf("home page without a sponsor lacks %q", want)
		}
	}
	if strings.Contains(body, `rel="sponsored noopener"`) || strings.Contains(body, "/go/sponsor") {
		t.Error("the house ad is tracked as if it were a sponsor click")
	}
}

func TestSponsorPageHasHonestFiguresAndNoSessions(t *testing.T) {
	visitors, asOfNow := 308, time.Now()
	audience := SponsorAudience{
		Days: 30, APICalls: 600, UpCalls: 500, APIClientDays: 120, AvgDailyClients: 4,
		HumanVisitors: &visitors, HumanSource: "Plausible", HumanAsOf: &asOfNow,
	}
	body := renderPage(t, SponsorPage(audience, SponsorSlot{}, "https://buy.example/checkout/abc", false))
	for _, want := range []string{
		"600", "500", "120", "4.0", "308", "client-days", "human visitors (Plausible)",
		`href="https://buy.example/checkout/abc"`, "$29", "never appears in the", "contact@supercapybara.com",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/sponsor lacks %q", want)
		}
	}
	// The raw server sessions count is the figure we refuse to sell on.
	if strings.Contains(strings.ToLower(body), "sessions per") || strings.Contains(body, "26517") {
		t.Error("/sponsor quotes the raw sessions count")
	}

	unreachable := renderPage(t, SponsorPage(SponsorAudience{Days: 30}, SponsorSlot{}, "", false))
	if !strings.Contains(unreachable, "n/a") || !strings.Contains(unreachable, "Plausible unreachable") {
		t.Error("an unreadable visitor figure must be shown as unavailable, not as zero")
	}
	if !strings.Contains(unreachable, "mailto:contact@supercapybara.com") {
		t.Error("with no checkout configured the call to action is email")
	}
}

func TestPlausibleReaderCachesAndFallsBackToTheLastGoodValue(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret-key" || r.URL.Query().Get("site_id") != "stillworks.example" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if fail.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"results":{"visitors":{"value":308},"pageviews":{"value":337}}}`))
	}))
	defer server.Close()

	if NewPlausibleReader(server.URL, "https://stillworks.example", "") != nil {
		t.Fatal("a reader was built with no key")
	}
	reader := NewPlausibleReader(server.URL, "https://stillworks.example", "secret-key")
	visitors, pageviews, _, ok := reader.Read(context.Background())
	if !ok || visitors != 308 || pageviews != 337 {
		t.Fatalf("read = %d/%d ok=%v", visitors, pageviews, ok)
	}
	reader.Read(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("second read within the cache window made %d upstream calls, want 1", calls.Load())
	}

	// Force a refresh against a failing upstream: the last good value is kept.
	reader.mu.Lock()
	reader.asOf = time.Now().Add(-time.Hour)
	reader.mu.Unlock()
	fail.Store(true)
	visitors, _, _, ok = reader.Read(context.Background())
	if !ok || visitors != 308 {
		t.Fatalf("with upstream down, read = %d ok=%v, want the last good value", visitors, ok)
	}

	if _, _, err := parsePlausibleAggregate([]byte(`{"results":{}}`)); err == nil {
		t.Fatal("an aggregate with no figures was accepted")
	}
}

func TestSponsorPageThanksAPaidVisitorOnlyWhenTheCheckoutSentThem(t *testing.T) {
	plain := renderPage(t, SponsorPage(SponsorAudience{Days: 30}, SponsorSlot{}, "https://buy.example/c", false))
	paid := renderPage(t, SponsorPage(SponsorAudience{Days: 30}, SponsorSlot{}, "https://buy.example/c", true))
	if strings.Contains(plain, "Payment received") || !strings.Contains(paid, "Payment received") {
		t.Fatal("the thank-you must appear for ?paid=1 and only then")
	}
}
