package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bon5co/godjango/database"
)

// What the slot costs, and how a sponsor pays. Both are read by the /sponsor
// page and by nothing else.
const (
	// SponsorPriceUSD is the flat monthly price. See the comparables in the
	// release notes for how it was set: the nearest published prices (a
	// 37,000-visit/month directory at $299, a 326,000-star README list at $99
	// for a text link) are for audiences one to three orders of magnitude
	// larger than this site's measured one, and an EthicalAds-style CPM over
	// this site's measured impressions is single-digit dollars a month. $29 is
	// priced against that evidence, with the slot's exclusivity and its place in
	// every API response as the part a CPM does not capture.
	SponsorPriceUSD = 29

	// SponsorContact is the public contact address. Not a credential.
	SponsorContact = "contact@supercapybara.com"

	// audienceDays is the window every audience figure is read over.
	audienceDays = 30
)

// sponsorCheckoutURL is the hosted checkout for the recurring monthly product.
// It is set from the environment at startup; empty means the page offers email
// instead of a pay button, and says so.
var sponsorCheckoutURL string

// SetSponsorCheckoutURL hands the page the checkout link. Startup only.
func SetSponsorCheckoutURL(raw string) {
	raw = strings.TrimSpace(raw)
	if raw != "" && validHTTPS(raw) != nil {
		slog.Warn("stillworks: SPONSOR_CHECKOUT_URL is not an absolute https URL; offering email instead")
		raw = ""
	}
	sponsorCheckoutURL = raw
}

// SponsorAudience is every figure /sponsor prints about who sees the slot.
//
// What is deliberately not here is the server's "sessions" count. Around 98% of
// it is automated traffic arriving with a spoofed Google referrer, and a
// sponsor sold on it would be buying an audience that is mostly not there.
// What is here is the part that is real: calls to the API by clients that did
// not identify as crawlers, and visitors a separate, bot-filtering analytics
// product counted.
type SponsorAudience struct {
	Days int `json:"window_days"`

	// APICalls is every API request in the window from a client that did not
	// identify as a crawler and was not our own. UpCalls is the subset that
	// asked for /api/llm/up, the response the slot is part of.
	APICalls int `json:"api_calls"`
	UpCalls  int `json:"api_calls_llm_up"`
	// APIClientDays counts distinct clients per UTC day and sums the days. A
	// client's identifier rotates at midnight by design, so the same program
	// calling on three days is counted three times here. It is "client-days",
	// and it is called that, because "distinct clients" would overstate it.
	APIClientDays int `json:"api_client_days"`
	// AvgDailyClients is APIClientDays over the window: how many distinct API
	// clients a typical day sees.
	AvgDailyClients float64 `json:"avg_daily_api_clients"`

	// Plausible figures. Nil means they could not be read, which is shown as
	// such and is not a zero.
	HumanVisitors  *int       `json:"human_visitors,omitempty"`
	HumanPageviews *int       `json:"human_pageviews,omitempty"`
	HumanSource    string     `json:"human_visitors_source"`
	HumanAsOf      *time.Time `json:"human_visitors_as_of,omitempty"`

	// Clicks on the current sponsor link in the window. Crawlers and our own
	// requests are excluded. FromPage is a link followed on the card; FromAPI
	// is a link followed from an /api/llm/up response, which is an agent or a
	// script. Neither is a claim about conversion.
	ClicksFromPage int `json:"sponsor_clicks_from_page"`
	ClicksFromAPI  int `json:"sponsor_clicks_from_api"`

	GeneratedAt time.Time `json:"generated_at"`
}

// LoadSponsorAudience reads the figures live from the traffic record.
func LoadSponsorAudience(ctx context.Context, db *database.DB, plausible *PlausibleReader) (SponsorAudience, error) {
	audience := SponsorAudience{Days: audienceDays, GeneratedAt: time.Now().UTC()}
	var row struct {
		APICalls      int `bun:"api_calls"`
		UpCalls       int `bun:"up_calls"`
		APIClientDays int `bun:"client_days"`
		FromPage      int `bun:"from_page"`
		FromAPI       int `bun:"from_api"`
	}
	err := db.Bun().NewRaw(`
		SELECT
		  count(*) FILTER (WHERE kind = 'api')                                     AS api_calls,
		  count(*) FILTER (WHERE kind = 'api' AND path = '/api/llm/up')            AS up_calls,
		  count(DISTINCT visitor_hash) FILTER (WHERE kind = 'api')                 AS client_days,
		  count(*) FILTER (WHERE kind = ? AND path = ?)                            AS from_page,
		  count(*) FILTER (WHERE kind = ? AND path = ?)                            AS from_api
		FROM traffic_events
		WHERE occurred_at > now() - make_interval(days => ?)
		  AND NOT is_crawler AND NOT is_internal
	`, KindSponsor, sponsorClickPath+"?src="+clickSourcePage,
		KindSponsor, sponsorClickPath+"?src="+clickSourceAPI, audienceDays).Scan(ctx, &row)
	if err != nil {
		return audience, err
	}
	audience.APICalls = row.APICalls
	audience.UpCalls = row.UpCalls
	audience.APIClientDays = row.APIClientDays
	audience.AvgDailyClients = float64(row.APIClientDays) / float64(audienceDays)
	audience.ClicksFromPage = row.FromPage
	audience.ClicksFromAPI = row.FromAPI

	audience.HumanSource = "unavailable"
	if plausible != nil {
		if visitors, pageviews, asOf, ok := plausible.Read(ctx); ok {
			audience.HumanVisitors = &visitors
			audience.HumanPageviews = &pageviews
			audience.HumanAsOf = &asOf
			audience.HumanSource = "Plausible, " + plausible.host + ", last 30 days"
		}
	}
	return audience, nil
}

// PlausibleReader reads this site's 30-day visitor count from the Plausible
// Stats API. Plausible filters known bots and counts without cookies; its
// number is the closest thing this site has to "people".
//
// Results are cached for half an hour, and the last good value is kept past
// that if a refresh fails: the page then says when it was measured instead of
// printing a number as if it were fresh, or dropping the figure.
type PlausibleReader struct {
	host   string
	site   string
	key    string
	client *http.Client

	mu        sync.Mutex
	visitors  int
	pageviews int
	asOf      time.Time
	ok        bool
	retryAt   time.Time
}

const (
	plausibleFreshFor = 30 * time.Minute
	plausibleRetryIn  = 2 * time.Minute
	plausibleMaxStale = 24 * time.Hour
)

// NewPlausibleReader returns nil when the deployment has no stats key, which
// the page renders as "unavailable" rather than as zero.
func NewPlausibleReader(host, publicOrigin, key string) *PlausibleReader {
	host, key = strings.TrimSuffix(strings.TrimSpace(host), "/"), strings.TrimSpace(key)
	if host == "" || key == "" || publicOrigin == "" {
		return nil
	}
	parsed, err := url.Parse(publicOrigin)
	if err != nil || parsed.Hostname() == "" {
		return nil
	}
	return &PlausibleReader{
		host: host, site: parsed.Hostname(), key: key,
		client: &http.Client{Timeout: 4 * time.Second},
	}
}

// Read returns the cached figures, refreshing them first if they are stale.
func (p *PlausibleReader) Read(ctx context.Context) (visitors, pageviews int, asOf time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if p.ok && now.Sub(p.asOf) < plausibleFreshFor {
		return p.visitors, p.pageviews, p.asOf, true
	}
	if now.Before(p.retryAt) {
		return p.stale(now)
	}
	v, pv, err := p.fetch(ctx)
	if err != nil {
		slog.Warn("stillworks: reading Plausible visitors failed", "error", err)
		p.retryAt = now.Add(plausibleRetryIn)
		return p.stale(now)
	}
	p.visitors, p.pageviews, p.asOf, p.ok = v, pv, now.UTC(), true
	return p.visitors, p.pageviews, p.asOf, true
}

func (p *PlausibleReader) stale(now time.Time) (int, int, time.Time, bool) {
	if p.ok && now.Sub(p.asOf) < plausibleMaxStale {
		return p.visitors, p.pageviews, p.asOf, true
	}
	return 0, 0, time.Time{}, false
}

func (p *PlausibleReader) fetch(ctx context.Context) (int, int, error) {
	query := url.Values{}
	query.Set("site_id", p.site)
	query.Set("period", "30d")
	query.Set("metrics", "visitors,pageviews")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.host+"/api/v1/stats/aggregate?"+query.Encode(), nil)
	if err != nil {
		return 0, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+p.key)
	response, err := p.client.Do(request)
	if err != nil {
		return 0, 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if err != nil {
		return 0, 0, err
	}
	if response.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("plausible answered %d", response.StatusCode)
	}
	return parsePlausibleAggregate(body)
}

// parsePlausibleAggregate reads {"results":{"visitors":{"value":N},...}}.
func parsePlausibleAggregate(body []byte) (int, int, error) {
	var decoded struct {
		Results struct {
			Visitors  *struct{ Value float64 } `json:"visitors"`
			Pageviews *struct{ Value float64 } `json:"pageviews"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return 0, 0, err
	}
	if decoded.Results.Visitors == nil || decoded.Results.Pageviews == nil {
		return 0, 0, fmt.Errorf("plausible response carried no visitors/pageviews")
	}
	return int(decoded.Results.Visitors.Value), int(decoded.Results.Pageviews.Value), nil
}
