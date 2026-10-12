package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bon5co/godjango/database"
)

// The pages that exist to be found, and what each one is for.
//
// Queries were chosen from live data on 2026-10-12 -- Google autocomplete and
// the top ten of a DuckDuckGo SERP for seven seed queries, with each result's
// title, meta description and h1 fetched. Google itself answered every session
// with a captcha, so the SERP half is DuckDuckGo's. What it showed:
//
//   - "free llm api" is crowded: GitHub awesome-lists, freellm.net, modelgrep,
//     lmspeed and a dozen 2026 listicles, most with titles over 60 characters.
//     The home page contests it on the one thing a list cannot do: it is
//     re-checked every hour, and says when.
//   - "free llm api no key" / "without api key" has thin autocomplete and a SERP
//     of tutorials and lists. Nobody in the top ten verifies the claim. That is
//     the keyless page.
//   - "free llm api resources github" is an autocomplete suggestion in several
//     spellings and the canonical repository behind it answered 404 on the day
//     we looked. That is the alternatives page.
//   - "free llm api for agents / claude code / opencode" is in autocomplete and
//     the audience is the one the runtime API is for. That is the docs page.
//   - "groq free tier limits", "openrouter free models" and the other provider
//     queries have ten near-identical blog posts each; the edge is a measured
//     page, so those are the provider pages' titles.
//
// Each of these is a content unit with its own text and its own numbers. None of
// them is a table row put through a template: the BetterLingo kanji pages were
// that, 2,212 of them, and earned two clicks.

const (
	homePath         = "/"
	keylessPagePath  = "/free-llm-api-no-key"
	alternativesPath = "/free-llm-api-resources-alternatives"
	docsPath         = "/docs"
)

// pageDef is one page's own head and its place in the sitemap.
type pageDef struct {
	Path string
	// Title is the <title>, 60 characters or fewer, primary keyword first.
	Title string
	// Heading is the h1. It carries the primary keyword too.
	Heading string
	// Description is the meta description, 155 characters or fewer.
	Description string
	// Short names the page in llms.txt and in breadcrumbs.
	Short string
	// Updated is the date the page's own words last changed.
	Updated time.Time
	// Published is the first date it was live, for Article markup.
	Published time.Time
	Card      string
	CardAlt   string
}

var (
	homeDef = pageDef{
		Path:    homePath,
		Title:   "Free LLM API List: Live-Tested Every Hour",
		Heading: "Free LLM API endpoints, tested every hour",
		Description: "Free LLM API endpoints called for real every hour, keyless first: no API key, no signup. " +
			"A ranked table and a JSON API that says which one answers now.",
		Short:     "Free LLM API list, live-tested",
		Updated:   day(2026, time.October, 12),
		Published: day(2026, time.August, 3),
	}
	keylessDef = pageDef{
		Path:    keylessPagePath,
		Title:   "Free LLM API With No API Key (Tested Hourly)",
		Heading: "Free LLM API with no API key",
		Description: "Which free LLM APIs answer with no API key and no signup right now. Each is called hourly with " +
			"no Authorization header: uptime, latency, a curl.",
		Short:     "Free LLM API with no API key",
		Updated:   day(2026, time.October, 12),
		Published: day(2026, time.October, 12),
		Card:      cardKeylessPath,
		CardAlt:   "stillworks: Free LLM API, no key. Keyless endpoints, called every hour, ranked.",
	}
	alternativesDef = pageDef{
		Path:    alternativesPath,
		Title:   "Free LLM API Resources (GitHub List) Alternative",
		Heading: "Free LLM API resources lists, and a live alternative",
		Description: "The GitHub free-LLM-API lists are snapshots, and the best known one answered 404 when we checked. " +
			"Here is what a list misses and what an hourly probe adds.",
		Short:     "Free LLM API resources list alternative",
		Updated:   day(2026, time.October, 12),
		Published: day(2026, time.October, 12),
		Card:      cardComparePath,
		CardAlt:   "stillworks: Lists rot. We re-check. Alternatives to static free-LLM lists.",
	}
	docsDef = pageDef{
		Path:    docsPath,
		Title:   "Free LLM API for Agents: /api/llm/up Reference",
		Heading: "Free LLM API for agents: the /api/llm/up reference",
		Description: "One GET returns a free LLM endpoint that answered recently: base URL, model, auth, latency, " +
			"proven features. Parameters, fields, errors, code.",
		Short:     "Free LLM API for agents: /api/llm/up reference",
		Updated:   day(2026, time.October, 12),
		Published: day(2026, time.October, 12),
		Card:      cardDocsPath,
		CardAlt:   "stillworks: one call, then you call. GET /api/llm/up returns base URL, model and auth.",
	}
)

// indexablePages is the route list the sitemap and llms.txt are built from. The
// handlers read their own definition from the same variables, so a page cannot
// be served as content and be missing here, or listed here and not served. The
// test that fetches every entry holds the router to it.
//
// Not in the list, on purpose: /stats and /api/* (noindex), /mcp/ (a "not open
// yet" page, noindex) and the three legacy shelf addresses, which are 301s.
func indexablePages() []sitePage {
	return []sitePage{
		{Path: homeDef.Path, Updated: homeDef.Updated},
		{Path: keylessDef.Path, Updated: keylessDef.Updated},
		{Path: alternativesDef.Path, Updated: alternativesDef.Updated},
		{Path: docsDef.Path, Updated: docsDef.Updated},
	}
}

func pageTitles() map[string]string {
	titles := map[string]string{}
	for _, def := range []pageDef{homeDef, keylessDef, alternativesDef, docsDef} {
		titles[def.Path] = def.Short
	}
	return titles
}

// clipMeta shortens text to at most limit characters on a word boundary, adding
// an ellipsis only when it cut. A search engine truncates a long description
// itself, mid-word; doing it here keeps the sentence whole.
func clipMeta(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	cut := string(runes[:limit-1])
	if space := strings.LastIndex(cut, " "); space > limit/2 {
		cut = cut[:space]
	}
	return strings.TrimRight(cut, " ,;:.-") + "…"
}

// providerHead is the title and h1 of a provider page. The wording names the
// query people type for that provider, which is what the page answers with
// measurements. A provider added later falls back to a template that still
// leads with the provider and the word "free".
func providerHead(row ShelfRow) (title string, heading string) {
	switch row.Slug {
	case "groq":
		return "Groq Free Tier Limits: Live Status & Uptime", "Groq free tier: limits we hit and what answered"
	case "openrouter":
		return "OpenRouter Free Models: Live Status Right Now", "OpenRouter free models: which ones answer now"
	case "mistral":
		return "Mistral Free API: Live Status & Measured Limits", "Mistral free API: what the free tier answered"
	case "llm7":
		return "LLM7 Free API, No Key: Live Status & Models", "LLM7 free API: the models that answer with no key"
	case "pollinations":
		return "Pollinations Free Text API, No Key: Live Status", "Pollinations free text API: no key, measured"
	case "ovh-anonymous":
		return "OVHcloud AI Endpoints Free Tier: No Key, Live", "OVHcloud AI Endpoints anonymous tier: no key, measured"
	case "mlvoca":
		return "mlvoca.com Free LLM API, No Key: Live Status", "mlvoca.com free LLM API: no key, measured"
	case "uncloseai":
		return "uncloseai Free LLM API, No Key: Live Status", "uncloseai free LLM API: no key, measured"
	}
	if row.RequiresKey() {
		return clipMeta(row.Provider+" Free API: Live Status & Uptime", 60), row.Provider + " free API: live status and uptime"
	}
	return clipMeta(row.Provider+" Free API, No Key: Live Status", 60), row.Provider + " free API, no key: live status"
}

// providerDescription is built from the numbers on the page, so it changes when
// they do and says something no other result for the same query can.
func providerDescription(row ShelfRow, track TrackRecord) string {
	kind := "answered with no key"
	if row.RequiresKey() {
		kind = "answered our free-tier key"
	}
	var text string
	if track.Attempts > 0 {
		text = fmt.Sprintf("%s: %d of %d models %s at the last check. %d of %d real chat calls answered in %s, median %d ms. Probe log and curl.",
			row.Provider, row.WorkingModels, row.ModelsListed, kind,
			track.Answered, track.Attempts, countOf(track.WindowDays, "day"), track.MedianMS)
	} else {
		text = fmt.Sprintf("%s: %d of %d models %s at the last check. Raw probe log and a working call.",
			row.Provider, row.WorkingModels, row.ModelsListed, kind)
	}
	return clipMeta(text, 155)
}

// ---- keyless page ----------------------------------------------------------

type keylessEntry struct {
	Row   ShelfRow
	Track TrackRecord
	// Best is the first verified model on the shelf's own ordering, or nil.
	Best *WorkingModel
}

func (h *handlers) keylessPage(response http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	rows, err := ShelfMatching(ctx, h.db, ParseShelfQuery(nil), AuthModeNone)
	if err != nil {
		serverError(response, request, err)
		return
	}
	working, err := WorkingModels(ctx, h.db, pageLimit, nil, AuthModeNone)
	if err != nil {
		serverError(response, request, err)
		return
	}
	entries := make([]keylessEntry, 0, len(rows))
	for _, row := range rows {
		track, err := EndpointTrackRecord(ctx, h.db, row.Slug)
		if err != nil {
			serverError(response, request, err)
			return
		}
		entry := keylessEntry{Row: row, Track: track}
		for index := range working {
			if working[index].Slug == row.Slug {
				entry.Best = &working[index]
				break
			}
		}
		entries = append(entries, entry)
	}
	def := keylessDef
	items := make([]any, 0, len(entries))
	for index, entry := range entries {
		items = append(items, map[string]any{
			"@type":    "ListItem",
			"position": index + 1,
			"url":      absolute(providerPath(entry.Row.Slug)),
			"name":     entry.Row.Provider,
		})
	}
	graph := append(websiteNodes(),
		map[string]any{
			"@type":         "CollectionPage",
			"@id":           absolute(def.Path) + "#page",
			"url":           absolute(def.Path),
			"name":          def.Heading,
			"description":   def.Description,
			"isPartOf":      map[string]any{"@id": absolute("/#site")},
			"dateModified":  time.Now().UTC().Format("2006-01-02"),
			"datePublished": def.Published.Format("2006-01-02"),
			"mainEntity": map[string]any{
				"@type":           "ItemList",
				"name":            "Free LLM API endpoints that answer with no API key",
				"numberOfItems":   len(items),
				"itemListElement": items,
			},
		},
		breadcrumb([2]string{"stillworks", "/"}, [2]string{def.Short, def.Path}),
	)
	render(response, request, page{
		Title:       def.Title,
		Description: def.Description,
		Canonical:   def.Path,
		Card:        def.Card,
		CardAlt:     def.CardAlt,
		Schema:      graph,
	}, KeylessPage(def, entries, working, lastVerified(working), apiOrigin()))
}

// ---- alternatives page -----------------------------------------------------

type alternativesData struct {
	KeylessModels int
	KeyedModels   int
	Providers     int
	Rejected      int
	Cycles14d     int
	Calls14d      int
}

func (h *handlers) alternativesPage(response http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	var data alternativesData
	keyless, err := WorkingModels(ctx, h.db, pageLimit, nil, AuthModeNone)
	if err != nil {
		serverError(response, request, err)
		return
	}
	both, err := WorkingModels(ctx, h.db, pageLimit, nil, AuthModeAny)
	if err != nil {
		serverError(response, request, err)
		return
	}
	data.KeylessModels = len(keyless)
	data.KeyedModels = len(both) - len(keyless)
	rows, err := ShelfMatching(ctx, h.db, ParseShelfQuery(nil), AuthModeAny)
	if err != nil {
		serverError(response, request, err)
		return
	}
	data.Providers = len(rows)
	data.Rejected = len(RejectedClaims)
	data.Calls14d, err = ProbeCount(ctx, h.db, TrackWindowDays)
	if err != nil {
		serverError(response, request, err)
		return
	}
	def := alternativesDef
	render(response, request, page{
		Title:       def.Title,
		Description: def.Description,
		Canonical:   def.Path,
		Card:        def.Card,
		CardAlt:     def.CardAlt,
		Article:     true,
		Schema:      articleGraph(def, "Article"),
	}, AlternativesPage(def, data, apiOrigin()))
}

// ProbeCount is how many probes of any kind were recorded in the last days.
// The alternatives page prints it as the size of the work a list does not do.
func ProbeCount(ctx context.Context, db *database.DB, days int) (int, error) {
	var count int
	err := db.Bun().NewRaw(
		`SELECT count(*) FROM llm_probes WHERE started_at > now() - make_interval(days => ?)`, days,
	).Scan(ctx, &count)
	return count, err
}

// ---- docs page --------------------------------------------------------------

func (h *handlers) docsPage(response http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	working, err := WorkingModels(ctx, h.db, 1, nil, AuthModeNone)
	if err != nil {
		serverError(response, request, err)
		return
	}
	example := "{ \"count\": 0, \"models\": [] }"
	if len(working) > 0 {
		encoded, err := json.MarshalIndent(working[0], "", "  ")
		if err == nil {
			example = string(encoded)
		}
	}
	def := docsDef
	render(response, request, page{
		Title:       def.Title,
		Description: def.Description,
		Canonical:   def.Path,
		Card:        def.Card,
		CardAlt:     def.CardAlt,
		Article:     true,
		Schema:      articleGraph(def, "TechArticle"),
	}, DocsPage(def, example, apiOrigin()))
}

// articleGraph is the JSON-LD for a written page: the shared publisher nodes,
// the article, and its breadcrumb.
func articleGraph(def pageDef, kind string) []any {
	return append(websiteNodes(),
		map[string]any{
			"@type":            kind,
			"@id":              absolute(def.Path) + "#article",
			"headline":         def.Heading,
			"description":      def.Description,
			"url":              absolute(def.Path),
			"mainEntityOfPage": absolute(def.Path),
			"datePublished":    def.Published.Format("2006-01-02"),
			"dateModified":     def.Updated.Format("2006-01-02"),
			"image":            absolute(def.Card),
			"author":           map[string]any{"@id": absolute("/#org")},
			"publisher":        map[string]any{"@id": absolute("/#org")},
			"isPartOf":         map[string]any{"@id": absolute("/#site")},
		},
		breadcrumb([2]string{"stillworks", "/"}, [2]string{def.Short, def.Path}),
	)
}

// homeGraph is what the home page is: a web application that answers one call,
// and the dataset of probe results behind it.
func homeGraph(modelsAnswering int) []any {
	return append(websiteNodes(),
		map[string]any{
			"@type":               "WebApplication",
			"@id":                 absolute("/#app"),
			"name":                siteName,
			"url":                 absolute("/"),
			"description":         homeDef.Description,
			"applicationCategory": "DeveloperApplication",
			"operatingSystem":     "Any",
			"isAccessibleForFree": true,
			"offers":              map[string]any{"@type": "Offer", "price": "0", "priceCurrency": "USD"},
			"publisher":           map[string]any{"@id": absolute("/#org")},
		},
		map[string]any{
			"@type": "Dataset",
			"@id":   absolute("/#dataset"),
			"name":  "Free LLM endpoint probe results",
			"description": fmt.Sprintf(
				"Real chat completions sent hourly to free and free-tier LLM endpoints, with outcome, HTTP status "+
					"and latency. %d models answered at the last sweep.", modelsAnswering),
			"url":                 absolute("/"),
			"isAccessibleForFree": true,
			"creator":             map[string]any{"@id": absolute("/#org")},
			"dateModified":        time.Now().UTC().Format("2006-01-02"),
			"measurementTechnique": "An HTTP chat completion request from the stillworks server, repeated hourly, " +
				"outcome recorded as ok, rate_limited, needs_key, server_error, timeout or unreachable.",
			"distribution": []any{
				map[string]any{
					"@type":          "DataDownload",
					"encodingFormat": "application/json",
					"contentUrl":     absolute("/api/llm/up"),
				},
			},
		},
	)
}

// answeringProviders is how many of the tracked keyless providers have at least
// one model answering right now.
func answeringProviders(entries []keylessEntry) int {
	count := 0
	for _, entry := range entries {
		if entry.Best != nil {
			count++
		}
	}
	return count
}
