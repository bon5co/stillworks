package audit

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/a-h/templ"
	"github.com/bon5co/godjango/web"
	"github.com/go-chi/chi/v5"
)

func allDefs() []pageDef { return []pageDef{homeDef, keylessDef, alternativesDef, docsDef, sponsorDef} }

// A title over 60 characters is truncated by a search engine, mid-phrase, and
// BetterLingo's checker page measurably earned the worst click-through on its
// site with a 74-character one. The limits are asserted rather than remembered.
func TestEveryPageFitsTheTitleAndDescriptionBudgets(t *testing.T) {
	for _, def := range allDefs() {
		if n := utf8.RuneCountInString(def.Title); n > 60 {
			t.Errorf("%s: title is %d characters, want <= 60: %q", def.Path, n, def.Title)
		}
		if n := utf8.RuneCountInString(def.Description); n > 155 || n < 70 {
			t.Errorf("%s: description is %d characters, want 70..155: %q", def.Path, n, def.Description)
		}
		if def.Heading == "" || def.Short == "" {
			t.Errorf("%s: heading and short name are required", def.Path)
		}
		if !strings.Contains(strings.ToLower(def.Title+def.Description+def.Heading), "free llm api") {
			t.Errorf("%s: the primary keyword is missing from title, description and h1", def.Path)
		}
	}
	for _, slug := range []string{"llm7", "pollinations", "ovh-anonymous", "mlvoca", "uncloseai", "groq", "mistral", "openrouter", "a-new-one"} {
		for _, auth := range []string{AuthModeNone, AuthModeKey} {
			title, heading := providerHead(ShelfRow{Slug: slug, Provider: "Example Provider", AuthMode: auth})
			if n := utf8.RuneCountInString(title); n > 60 {
				t.Errorf("provider %s (%s): title is %d characters: %q", slug, auth, n, title)
			}
			if heading == "" || !strings.Contains(strings.ToLower(title), "free") {
				t.Errorf("provider %s: title %q must lead with the free-tier query", slug, title)
			}
		}
	}
	long := providerDescription(ShelfRow{Provider: strings.Repeat("Provider ", 30), ModelsListed: 3},
		TrackRecord{Attempts: 10, Answered: 9, WindowDays: 14, MedianMS: 300})
	if utf8.RuneCountInString(long) > 155 {
		t.Errorf("provider description is %d characters", utf8.RuneCountInString(long))
	}
}

// The sitemap, llms.txt and the router must agree. This walks the real route
// table: every page in the list has a GET handler, no redirect or noindex route
// is in it, and a page that is served as content is in it.
func TestIndexablePagesAreExactlyTheRoutesThatServeContent(t *testing.T) {
	router := chi.NewRouter()
	New().RoutesWithServices(router, web.RuntimeServices{})
	registered := map[string]bool{}
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet {
			registered[route] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range indexablePages() {
		if !registered[entry.Path] {
			t.Errorf("%s is in the sitemap list but no GET route serves it", entry.Path)
		}
		if seen[entry.Path] {
			t.Errorf("%s is listed twice", entry.Path)
		}
		seen[entry.Path] = true
		if entry.Updated.IsZero() || entry.Updated.After(time.Now().Add(24*time.Hour)) {
			t.Errorf("%s has a nonsense lastmod %v", entry.Path, entry.Updated)
		}
	}
	for _, forbidden := range []string{"/stats", "/mcp/", "/llm/", "/llm/keyed/", "/llm/try", "/api/llm/up", "/robots.txt", "/sitemap.xml"} {
		if seen[forbidden] {
			t.Errorf("%s must not be in the sitemap", forbidden)
		}
	}
}

func TestSitemapCarriesEveryPageAndProviderWithADateLastmod(t *testing.T) {
	providers := []providerPage{
		{Slug: "groq", LastProbe: time.Date(2026, 10, 12, 3, 4, 5, 0, time.UTC)},
		{Slug: "llm7", LastProbe: time.Date(2026, 10, 11, 23, 59, 0, 0, time.UTC)},
	}
	body, err := buildSitemap("https://example.test", indexablePages(), providers)
	if err != nil {
		t.Fatal(err)
	}
	var parsed sitemapDocument
	if err := xml.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("sitemap is not valid XML: %v\n%s", err, body)
	}
	if want := len(indexablePages()) + len(providers); len(parsed.URLs) != want {
		t.Fatalf("sitemap has %d urls, want %d", len(parsed.URLs), want)
	}
	for _, entry := range parsed.URLs {
		if !strings.HasPrefix(entry.Loc, "https://example.test/") {
			t.Errorf("loc %q is not absolute on the configured origin", entry.Loc)
		}
		if _, err := time.Parse("2006-01-02", entry.Lastmod); err != nil {
			t.Errorf("%s: lastmod %q is not a date", entry.Loc, entry.Lastmod)
		}
	}
	if !strings.Contains(string(body), "<loc>https://example.test/llm/groq</loc>") ||
		!strings.Contains(string(body), "<lastmod>2026-10-12</lastmod>") {
		t.Errorf("provider entry missing:\n%s", body)
	}
}

func TestRobotsNamesTheSitemapAndLlmsTxtListsEveryPage(t *testing.T) {
	robots := robotsTxt("https://example.test")
	if !strings.Contains(robots, "\nSitemap: https://example.test/sitemap.xml\n") {
		t.Errorf("robots.txt has no Sitemap line:\n%s", robots)
	}
	if strings.Contains(robots, "Disallow: /\n") || strings.Contains(robots, "Disallow: /stats") {
		t.Errorf("robots.txt must not forbid the pages that are kept out by noindex:\n%s", robots)
	}
	text := llmsTxt("https://example.test", indexablePages(), pageTitles(), []providerPage{{Slug: "groq"}})
	for _, entry := range indexablePages() {
		if !strings.Contains(text, "https://example.test"+entry.Path) {
			t.Errorf("llms.txt is missing %s", entry.Path)
		}
	}
	for _, want := range []string{"/api/llm/up", "/llm/groq", "# stillworks"} {
		if !strings.Contains(text, want) {
			t.Errorf("llms.txt is missing %q", want)
		}
	}
}

// Each page publishes a canonical equal to its own address, the home page
// included, and a card of its own where it declared one.
func TestEachDefinitionRendersASelfCanonicalAndItsOwnCard(t *testing.T) {
	previous := publicOrigin
	defer SetPublicOrigin(previous)
	SetPublicOrigin("https://example.test")
	for _, def := range allDefs() {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, def.Path, nil)
		render(response, request, page{
			Title: def.Title, Description: def.Description, Canonical: def.Path,
			Card: def.Card, CardAlt: def.CardAlt, Article: def.Card != "" && def.Path != keylessPagePath,
		}, NotFoundPage("x"))
		body := response.Body.String()
		if !strings.Contains(body, `<link rel="canonical" href="https://example.test`+def.Path+`">`) {
			t.Errorf("%s: no self-canonical:\n%s", def.Path, head(body))
		}
		wantCard := def.Card
		if wantCard == "" {
			wantCard = socialCardPath
		}
		if !strings.Contains(body, `og:image" content="https://example.test`+wantCard+`"`) {
			t.Errorf("%s: wrong og:image:\n%s", def.Path, head(body))
		}
	}
}

func TestNoIndexIsAHeaderAndSchemaIsValidJSONThatCannotCloseItsScript(t *testing.T) {
	previous := publicOrigin
	defer SetPublicOrigin(previous)
	SetPublicOrigin("https://example.test")

	response := httptest.NewRecorder()
	render(response, httptest.NewRequest(http.MethodGet, "/stats", nil), page{Title: "Traffic", NoIndex: true}, NotFoundPage("x"))
	if got := response.Header().Get("X-Robots-Tag"); got != "noindex, follow" {
		t.Errorf("X-Robots-Tag = %q", got)
	}

	hostile := []any{map[string]any{"@type": "Thing", "name": "</script><script>alert(1)</script> & co"}}
	response = httptest.NewRecorder()
	render(response, httptest.NewRequest(http.MethodGet, "/", nil), page{
		Title: "T", Description: "a description long enough to be published", Canonical: "/", Schema: hostile,
	}, templ.Raw("<p>body</p>"))
	body := response.Body.String()
	if strings.Count(body, "</script>") != strings.Count(body, "<script") {
		t.Fatalf("schema closed its own script element early:\n%s", body)
	}
	start := strings.Index(body, `<script type="application/ld+json">`)
	if start < 0 {
		t.Fatal("no JSON-LD block was written")
	}
	block := body[start+len(`<script type="application/ld+json">`):]
	block = block[:strings.Index(block, "</script>")]
	var parsed map[string]any
	if err := json.Unmarshal([]byte(block), &parsed); err != nil {
		t.Fatalf("JSON-LD does not parse: %v\n%s", err, block)
	}
	if parsed["@context"] != "https://schema.org" {
		t.Errorf("@context = %v", parsed["@context"])
	}
}

func TestJSONAPIAnswersAreKeptOutOfResultsByHeader(t *testing.T) {
	response := httptest.NewRecorder()
	writeJSON(response, http.StatusOK, map[string]any{"ok": true})
	if got := response.Header().Get("X-Robots-Tag"); got != "noindex" {
		t.Errorf("X-Robots-Tag = %q, want noindex", got)
	}
}

func TestTrackRecordArithmeticAndSentences(t *testing.T) {
	median, p95 := 349.6, 1200.2
	since := time.Now().Add(-30 * 24 * time.Hour)
	record := assembleTrackRecord(
		[]trackAggregate{{Attempts: 100, Answered: 90, RateLimited: 6, NeedsKey: 2, ServerError: 1,
			MedianMS: &median, P95MS: &p95, Since: &since}},
		[]trackDayRow{{Day: time.Now(), Attempts: 10, Answered: 9, MedianMS: &median}},
	)
	if record.Other != 1 || record.MedianMS != 350 || record.P95MS != 1200 || record.WindowDays != TrackWindowDays {
		t.Errorf("unexpected record: %+v", record)
	}
	if got := record.Sentence(); !strings.Contains(got, "90 of 100") || !strings.Contains(got, "14 days") ||
		!strings.Contains(got, "90%") {
		t.Errorf("sentence = %q", got)
	}
	refusals := record.RefusalSentence()
	for _, want := range []string{"10 that did not answer", "6 rate-limited", "2 refused for a missing", "1 server errors", "1 other"} {
		if !strings.Contains(refusals, want) {
			t.Errorf("refusal sentence %q lacks %q", refusals, want)
		}
	}
	// An endpoint watched for a day must not claim a fortnight of record.
	recent := time.Now().Add(-2 * time.Hour)
	young := assembleTrackRecord([]trackAggregate{{Attempts: 2, Answered: 2, MedianMS: &median, Since: &recent}}, nil)
	if young.WindowDays != 1 || !strings.Contains(young.Sentence(), "in 1 day ") {
		t.Errorf("young endpoint claims %d days: %q", young.WindowDays, young.Sentence())
	}
	empty := assembleTrackRecord(nil, nil)
	if empty.Rate() != -1 || !strings.Contains(empty.Sentence(), "No chat call") || empty.RefusalSentence() != "" {
		t.Errorf("empty record: %q", empty.Sentence())
	}
}

func TestClipMetaCutsOnAWordAndOnlyWhenNeeded(t *testing.T) {
	if got := clipMeta("short  and   sweet", 50); got != "short and sweet" {
		t.Errorf("got %q", got)
	}
	got := clipMeta(strings.Repeat("word ", 60), 40)
	if utf8.RuneCountInString(got) > 40 || !strings.HasSuffix(got, "…") || strings.Contains(got, "wor…") {
		t.Errorf("got %q", got)
	}
}

// Days before the first probe are not gaps in our measurement, they are days
// nobody was watching, and the table must not list them as "not probed".
func TestTrackDaysBeforeWatchingBeganAreDropped(t *testing.T) {
	now := time.Now().UTC().Truncate(24 * time.Hour)
	since := now.Add(-48*time.Hour + time.Hour)
	median := 100.0
	days := []trackDayRow{{Day: now.Add(-72 * time.Hour)}, {Day: now.Add(-48 * time.Hour), Attempts: 1, Answered: 1, MedianMS: &median}, {Day: now.Add(-24 * time.Hour)}, {Day: now, Attempts: 2, Answered: 2}}
	record := assembleTrackRecord([]trackAggregate{{Attempts: 3, Answered: 3, MedianMS: &median, Since: &since}}, days)
	if len(record.Days) != 3 {
		t.Fatalf("got %d day rows, want 3 (the day before the first probe dropped, a later empty day kept)", len(record.Days))
	}
}

// The real seed rows, not a stand-in name: a long provider name is what pushes
// a fallback title past the budget.
func TestEverySeededProviderHasATitleWithinBudgetThatLeadsWithItsName(t *testing.T) {
	for _, endpoint := range SeedEndpoints {
		row := ShelfRow{Slug: endpoint.Slug, Provider: endpoint.Provider, AuthMode: endpoint.AuthMode}
		title, heading := providerHead(row)
		if n := utf8.RuneCountInString(title); n > 60 {
			t.Errorf("%s: title is %d characters: %q", endpoint.Slug, n, title)
		}
		name := strings.ToLower(endpoint.Provider)
		name = name[:min(len(name), 4)]
		if !strings.Contains(strings.ToLower(title), name) || heading == "" {
			t.Errorf("%s: title %q does not name the provider", endpoint.Slug, title)
		}
	}
}

// Groq lists models it refuses with a 404 for this key, and those calls are
// 40% of the attempts. Charging the provider for them as downtime printed 58%
// uptime for an endpoint that answered 99% of what it accepted. The two figures
// are kept apart.
func TestRefusalsForAModelAreNotCountedAsTheProviderBeingDown(t *testing.T) {
	median := 219.0
	record := assembleTrackRecord([]trackAggregate{{
		Attempts: 1014, Answered: 589, ClientError: 422, ServerError: 3, MedianMS: &median,
	}}, nil)
	if record.Other != 0 || record.Accepted() != 592 {
		t.Fatalf("other=%d accepted=%d, want 0 and 592", record.Other, record.Accepted())
	}
	if got := record.AcceptedSentence(); !strings.Contains(got, "589 answered (99%)") || !strings.Contains(got, "not the provider being down") {
		t.Errorf("accepted sentence = %q", got)
	}
	if got := record.RefusalSentence(); !strings.Contains(got, "422 refused for the one model asked about") {
		t.Errorf("refusal sentence = %q", got)
	}
	row := ShelfRow{Provider: "Groq", AuthMode: AuthModeKey, ModelsListed: 46, WorkingModels: 40}
	description := providerDescription(row, record)
	if !strings.Contains(description, "99% of the calls it accepted") || utf8.RuneCountInString(description) > 155 {
		t.Errorf("description = %q (%d)", description, utf8.RuneCountInString(description))
	}
	clean := assembleTrackRecord([]trackAggregate{{Attempts: 10, Answered: 10, MedianMS: &median}}, nil)
	if clean.AcceptedSentence() != "" {
		t.Errorf("a clean record needs no accepted sentence: %q", clean.AcceptedSentence())
	}
}

func TestModelsAnsweringNeverPrintsMoreAnsweringThanListed(t *testing.T) {
	for _, c := range []struct {
		working, listed int
		want            string
	}{{3, 23, "3 of 23 models"}, {12, 11, "12 models"}, {1, 1, "1 of 1 model"}, {0, 0, "0 of 0 models"}, {1, 0, "1 model"}} {
		if got := modelsAnswering(c.working, c.listed); got != c.want {
			t.Errorf("modelsAnswering(%d,%d) = %q, want %q", c.working, c.listed, got, c.want)
		}
	}
}

func TestSearchConsoleVerificationFileIsServedAtRootAndKeptOutOfTheSitemap(t *testing.T) {
	router := chi.NewRouter()
	New().RoutesWithServices(router, web.RuntimeServices{})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, searchConsoleVerificationPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := response.Body.String(); got != "google-site-verification: google29820619057a699c.html" {
		t.Errorf("body = %q", got)
	}
	for _, entry := range indexablePages() {
		if entry.Path == searchConsoleVerificationPath {
			t.Errorf("%s must not be in the sitemap", entry.Path)
		}
	}
}
