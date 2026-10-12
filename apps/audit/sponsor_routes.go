package audit

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
)

// sponsorPlausible is the visitor-count reader /sponsor uses, set once at
// startup. Nil means the figure is shown as unavailable.
var sponsorPlausible *PlausibleReader

// SetSponsorPlausible wires the Plausible Stats API reader. host is the
// Plausible instance, origin the public origin (whose hostname is the site name
// Plausible files this site under) and key a Stats API key. Any being empty
// leaves the figure unavailable, which the page says in words.
func SetSponsorPlausible(host, origin, key string) {
	sponsorPlausible = NewPlausibleReader(host, origin, key)
}

// sponsorSlot loads the slot for today. A failure is logged and answered with
// whatever was read -- at worst the house ad -- because a broken sponsor lookup
// must not take the shelf or the API down with it.
func (h *handlers) sponsorSlot(ctx context.Context) SponsorSlot {
	slot, err := LoadSponsorSlot(ctx, h.db, time.Now())
	if err != nil {
		slog.Error("stillworks: loading the sponsored slot failed; showing what was read", "error", err)
	}
	return slot
}

// goSponsor is the first-party redirect behind every sponsor link.
//
// It records the click and answers 302 to the sponsor's destination. With no
// sponsor active it sends the visitor to /sponsor instead, and records nothing:
// a click on a house ad is not a sponsor click.
//
// The source query value is one of two known strings and anything else is filed
// as "page", so the traffic table cannot be filled with caller-chosen text.
func (h *handlers) goSponsor(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("X-Robots-Tag", "noindex, nofollow")
	response.Header().Set("Cache-Control", "no-store")
	slot := h.sponsorSlot(request.Context())
	if slot.Sponsor == nil {
		http.Redirect(response, request, sponsorPagePath, http.StatusFound)
		return
	}
	source := clickSourcePage
	if request.URL.Query().Get("src") == clickSourceAPI {
		source = clickSourceAPI
	}
	h.recorder.RecordAs(request, KindSponsor, sponsorClickPath+"?src="+source, http.StatusFound)
	http.Redirect(response, request, slot.Sponsor.URL, http.StatusFound)
}

func (h *handlers) sponsorPage(response http.ResponseWriter, request *http.Request) {
	audience, err := LoadSponsorAudience(request.Context(), h.db, h.plausible)
	if err != nil {
		serverError(response, request, err)
		return
	}
	slot := h.sponsorSlot(request.Context())
	render(response, request, page{
		Title: sponsorPageTitle(),
		Description: "One sponsored slot on a live-probed free LLM registry: on the page and in every " +
			"/api/llm/up response. Flat monthly price, honest audience figures read live.",
		Canonical: sponsorPagePath,
	}, SponsorPage(audience, slot, sponsorCheckoutURL, request.URL.Query().Get("paid") == "1"))
}

// apiSponsor publishes the same figures the page prints, as JSON, so a sponsor's
// agent can read the audience without scraping.
func (h *handlers) apiSponsor(response http.ResponseWriter, request *http.Request) {
	audience, err := LoadSponsorAudience(request.Context(), h.db, h.plausible)
	if err != nil {
		apiError(response, err)
		return
	}
	slot := h.sponsorSlot(request.Context())
	var current *SponsoredEntry
	if slot.Sponsor != nil {
		current = NewSponsoredEntry(*slot.Sponsor, slot.Probe, apiOrigin())
	}
	checkout := any(nil)
	if sponsorCheckoutURL != "" {
		checkout = sponsorCheckoutURL
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"price_usd_per_month": SponsorPriceUSD,
		"billing":             "recurring monthly, cancel any time",
		"checkout_url":        checkout,
		"contact":             SponsorContact,
		"audience":            audience,
		"audience_note": "API figures come from our own request log with crawlers and our own requests " +
			"removed. api_client_days sums distinct clients per UTC day, so one program calling on three " +
			"days counts three times. Human visitors are read from Plausible, which filters bots. The " +
			"server's raw 'sessions' count is not an audience figure and is not used.",
		"current": current,
	})
}

// sponsorClickHref is the tracked link the page card uses.
func sponsorClickHref() templ.SafeURL {
	return templ.SafeURL(sponsorClickPath + "?src=" + clickSourcePage)
}

// mailtoSponsor is the contact link with the subject filled in.
func mailtoSponsor() templ.SafeURL {
	return templ.SafeURL("mailto:" + SponsorContact + "?subject=" +
		strings.ReplaceAll("stillworks sponsor slot", " ", "%20"))
}

// sponsorPageTitle stays under 60 characters, the limit a search result shows.
func sponsorPageTitle() string {
	return fmt.Sprintf("Sponsor stillworks — one slot, $%d/month", SponsorPriceUSD)
}
