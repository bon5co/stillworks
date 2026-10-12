package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bon5co/godjango/database"
)

// What search engines and agents are told about this site, in one place.
//
// The rule that shapes this file: the sitemap, the robots file, llms.txt and the
// pages themselves are all built from the same route list. A hand-written
// sitemap is a second list, and a second list is how a page ships without being
// in it -- BetterLingo's graded reading stories earned impressions for weeks
// while missing from its sitemap, and its kanji pages were in it and earned
// nothing. Neither mistake is made by construction here: a page that exists as
// content is in sitePages, which is what the handler, the sitemap, llms.txt and
// the test that fetches every entry all read.

//go:embed static/card-keyless.png
var cardKeylessPNG []byte

//go:embed static/card-provider.png
var cardProviderPNG []byte

//go:embed static/card-compare.png
var cardComparePNG []byte

//go:embed static/card-docs.png
var cardDocsPNG []byte

// One 1200x630 card per template. The same mark and palette as the site card,
// so a pasted link is recognisably this site, and a different line of text so
// that a feed holding three of our links does not show three identical tiles.
// Served from this origin, never a CDN: the CSP is default-src 'self', and a
// scraper that cannot fetch the image renders a blank rectangle instead.
var (
	cardKeylessPath  = fmt.Sprintf("/static/stillworks/card-keyless.%x.png", sha256.Sum224(cardKeylessPNG))
	cardProviderPath = fmt.Sprintf("/static/stillworks/card-provider.%x.png", sha256.Sum224(cardProviderPNG))
	cardComparePath  = fmt.Sprintf("/static/stillworks/card-compare.%x.png", sha256.Sum224(cardComparePNG))
	cardDocsPath     = fmt.Sprintf("/static/stillworks/card-docs.%x.png", sha256.Sum224(cardDocsPNG))
)

const defaultCardAlt = "stillworks: a row of sample squares, most of them verified green, " +
	"one refused, over the words \"directories list, we check\"."

func serveCard(body []byte, name string) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "image/png")
		response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeContent(response, request, name, time.Time{}, bytes.NewReader(body))
	}
}

// siteName and siteTagline are the strings that name the site in structured
// data. The tagline is what the site is, in the words a searcher uses.
const (
	siteName    = "stillworks"
	siteTagline = "Free LLM API endpoints, re-tested every hour by real calls"
)

// sitePage is one indexable page that is not a provider page: its address, the
// date its text last changed, and what it is. Provider pages are generated from
// the database and join the same list in sitemapEntries.
//
// Updated is the date the page's own words last changed, and it is a literal
// date in the source on purpose. A lastmod that is the time of the request says
// every page changed a second ago, every time, and a crawler learns within a
// few weeks to ignore the field for the whole site.
type sitePage struct {
	Path    string
	Updated time.Time
	// Hidden keeps a page that is served and linked out of llms.txt. Everything
	// in sitePages is in the sitemap.
	Hidden bool
}

func day(year int, month time.Month, d int) time.Time {
	return time.Date(year, month, d, 0, 0, 0, 0, time.UTC)
}

// sitemapEntry is one <url>. Lastmod is a date, W3C datetime at day precision.
type sitemapEntry struct {
	Loc     string `xml:"loc"`
	Lastmod string `xml:"lastmod,omitempty"`
}

type sitemapDocument struct {
	XMLName xml.Name       `xml:"urlset"`
	XMLNS   string         `xml:"xmlns,attr"`
	URLs    []sitemapEntry `xml:"url"`
}

// providerPage is what the sitemap needs to know about one endpoint's page.
type providerPage struct {
	Slug string
	// LastProbe is the newest probe of any kind. The provider page prints the
	// probe log, so this is the honest last-modified time of its content.
	LastProbe time.Time
}

// providerPages lists the active endpoints with the time of their newest probe.
func providerPages(ctx context.Context, db *database.DB) ([]providerPage, error) {
	var rows []providerPage
	err := db.Bun().NewRaw(`
		SELECT e.slug AS slug,
		       COALESCE(max(p.started_at), e.updated_at) AS last_probe
		FROM llm_endpoints e
		LEFT JOIN llm_probes p ON p.endpoint_id = e.id
		WHERE e.active
		GROUP BY e.slug, e.updated_at
		ORDER BY e.slug
	`).Scan(ctx, &rows)
	return rows, err
}

// buildSitemap is pure: pages and providers in, the document out, so a test can
// hold it against the router without a database.
func buildSitemap(origin string, pages []sitePage, providers []providerPage) ([]byte, error) {
	document := sitemapDocument{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, entry := range pages {
		document.URLs = append(document.URLs, sitemapEntry{
			Loc:     origin + entry.Path,
			Lastmod: entry.Updated.UTC().Format("2006-01-02"),
		})
	}
	for _, provider := range providers {
		document.URLs = append(document.URLs, sitemapEntry{
			Loc:     origin + providerPath(provider.Slug),
			Lastmod: provider.LastProbe.UTC().Format("2006-01-02"),
		})
	}
	var buffer bytes.Buffer
	buffer.WriteString(xml.Header)
	encoder := xml.NewEncoder(&buffer)
	encoder.Indent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	buffer.WriteByte('\n')
	return buffer.Bytes(), nil
}

func providerPath(slug string) string { return "/llm/" + slug }

func (h *handlers) sitemap(response http.ResponseWriter, request *http.Request) {
	providers, err := providerPages(request.Context(), h.db)
	if err != nil {
		http.Error(response, "sitemap unavailable", http.StatusInternalServerError)
		return
	}
	body, err := buildSitemap(apiOrigin(), indexablePages(), providers)
	if err != nil {
		http.Error(response, "sitemap unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/xml; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = response.Write(body)
}

// robotsTxt allows everything and names the one route that makes an outbound
// call on a visitor's behalf. /stats and the JSON API are deliberately NOT
// disallowed: they are kept out of results by an X-Robots-Tag noindex header,
// and a crawler that is forbidden to fetch a URL never reads that header, so a
// Disallow would leave exactly the listing-without-a-snippet it is meant to
// prevent whenever another site links there.
//
// AI crawlers are not blocked either. The audience is developers and the
// agents they run, and being readable by the assistants people ask "which free
// LLM API works right now" is the distribution this site wants.
func robotsTxt(origin string) string {
	return "User-agent: *\n" +
		"Allow: /\n" +
		"Disallow: /api/llm/try\n" +
		"\n" +
		"Sitemap: " + origin + "/sitemap.xml\n"
}

func (h *handlers) robots(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(response, robotsTxt(apiOrigin()))
}

// withSchema writes the JSON-LD block ahead of a page's own content. json.Marshal
// escapes <, > and & as unicode, so nothing in the data can close the script
// element early.
func withSchema(graph []any, content templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, writer io.Writer) error {
		encoded, err := json.Marshal(map[string]any{"@context": "https://schema.org", "@graph": graph})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(writer, `<script type="application/ld+json">%s</script>`, encoded); err != nil {
			return err
		}
		return content.Render(ctx, writer)
	})
}

// absolute turns a site path into a URL on the configured origin.
func absolute(path string) string { return apiOrigin() + path }

// breadcrumb is a BreadcrumbList for a page one level under the home page.
func breadcrumb(items ...[2]string) map[string]any {
	list := make([]any, 0, len(items))
	for index, item := range items {
		list = append(list, map[string]any{
			"@type":    "ListItem",
			"position": index + 1,
			"name":     item[0],
			"item":     absolute(item[1]),
		})
	}
	return map[string]any{"@type": "BreadcrumbList", "itemListElement": list}
}

// organisation and website are the two nodes every page's graph shares, so the
// publisher of a guide is the same entity as the owner of the tool.
func websiteNodes() []any {
	return []any{
		map[string]any{
			"@type": "Organization",
			"@id":   absolute("/#org"),
			"name":  siteName,
			"url":   absolute("/"),
			"logo":  absolute(faviconPath),
		},
		map[string]any{
			"@type":     "WebSite",
			"@id":       absolute("/#site"),
			"url":       absolute("/"),
			"name":      siteName,
			"publisher": map[string]any{"@id": absolute("/#org")},
		},
	}
}

// llmsTxt is the plain-text map of the site for an assistant or an agent: what
// it is, the call that answers the usual question, and where each page is. It
// is generated from the same list as the sitemap.
func llmsTxt(origin string, pages []sitePage, titles map[string]string, providers []providerPage) string {
	var out strings.Builder
	out.WriteString("# stillworks\n\n")
	out.WriteString("> " + siteTagline + ". A registry of free and free-tier LLM API endpoints. " +
		"Every row is a real chat call made from our server, published with the time it was measured. " +
		"Keyless endpoints (no API key, no signup) are listed first.\n\n")
	out.WriteString("## Ask what works right now (JSON, no key, CORS open)\n\n")
	out.WriteString("- [GET " + origin + "/api/llm/up](" + origin + "/api/llm/up): keyless models that answered recently, ranked; `models[0]` is the pick.\n")
	out.WriteString("- [GET " + origin + "/api/llm/up?key=any](" + origin + "/api/llm/up?key=any): the same, plus free-tier providers that need your own key.\n")
	out.WriteString("- [GET " + origin + "/api/llm/up?feature=tools,vision](" + origin + "/api/llm/up?feature=tools,vision): only models a real call proved can do those things. Features: " + strings.Join(Capabilities, ", ") + ".\n")
	out.WriteString("- [GET " + origin + "/api/llm/up?format=env](" + origin + "/api/llm/up?format=env): three lines, `OPENAI_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_MODEL`.\n\n")
	out.WriteString("The same response carries a separate top-level `sponsored` field: null, or one labelled paid placement. " +
		"It is never inside `models` and never changes their order, so `models[0]` is still the pick. See " + origin + "/sponsor.\n\n")
	out.WriteString("## Pages\n\n")
	for _, page := range pages {
		if page.Hidden {
			continue
		}
		title := titles[page.Path]
		if title == "" {
			title = page.Path
		}
		out.WriteString("- [" + title + "](" + origin + page.Path + ")\n")
	}
	out.WriteString("\n## Providers (one page each, with track record and probe log)\n\n")
	slugs := make([]string, 0, len(providers))
	for _, provider := range providers {
		slugs = append(slugs, provider.Slug)
	}
	slices.Sort(slugs)
	for _, slug := range slugs {
		out.WriteString("- [" + slug + "](" + origin + providerPath(slug) + ")\n")
	}
	out.WriteString("\n## Caveats\n\n")
	out.WriteString("Keyless quotas are usually per IP: an endpoint verified from our address can still answer 402 or 429 from yours. " +
		"These are other people's free services and can add a key requirement or disappear between two probes.\n")
	return out.String()
}

func (h *handlers) llms(response http.ResponseWriter, request *http.Request) {
	providers, err := providerPages(request.Context(), h.db)
	if err != nil {
		http.Error(response, "llms.txt unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(response, llmsTxt(apiOrigin(), indexablePages(), pageTitles(), providers))
}
