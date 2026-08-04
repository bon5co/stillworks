package audit

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Plausible runs alongside this site's own traffic recorder rather than
// instead of it. The recorder answers "what did this project's own append-only
// log record", which is the number the outcome contract is checked against and
// which no ad-blocker can take away; Plausible answers the questions a log of
// requests cannot -- which channel a visitor came from over a month, what they
// read next -- and it does it in a dashboard nobody has to be handed a SQL
// prompt to read. Neither replaces the other, and where they disagree the
// recorder is the one that counts.
//
// Everything Plausible needs is served from this origin. That is not a
// preference: the site's Content-Security-Policy is default-src 'self', so a
// <script src="https://plausible.../js/script.js"> is dropped by the browser
// before it runs. No error a visitor sees, no error in our logs, no pageviews,
// and a page that looks exactly right -- which is the single most expensive
// shape of failure this project has a name for. Proxying keeps the policy
// untouched and, as a second-order benefit, survives the blocklists that match
// on the analytics vendor's hostname.

//go:embed static/plausible.js
var plausibleJS []byte

// analyticsScriptPath carries the vendored script's content hash for the same
// reason the stylesheet's does: it can then be cached forever and still change
// when the file does. The name says nothing about analytics -- a path with
// "plausible" in it is matched by the same blocklists proxying was meant to
// step around.
var analyticsScriptPath = fmt.Sprintf("/js/p.%x.js", sha256.Sum224(plausibleJS))

// AnalyticsEventPath is where the browser posts and where this service forwards
// from. It is on this origin, so connect-src 'self' already permits it and the
// CSP does not move.
const AnalyticsEventPath = "/api/event"

const (
	// analyticsBodyLimit bounds what will be relayed. A pageview is a few
	// hundred bytes; anything near this is not one.
	analyticsBodyLimit = 8 << 10
	// analyticsTimeout keeps a slow upstream from holding a connection of ours
	// open. The browser has already painted by the time this runs, so the only
	// thing a longer wait buys is a stuck goroutine.
	analyticsTimeout = 5 * time.Second
)

// Analytics is the Plausible integration: two static files and one forwarder,
// all on this origin.
type Analytics struct {
	// eventURL is the upstream ingestion endpoint.
	eventURL string
	// domain is the site name registered in Plausible. It has to match exactly
	// or the event is accepted with a 202 and then dropped, which is the one
	// failure mode in this file that leaves no trace anywhere.
	domain string
	// internal is the same set of addresses the traffic recorder excludes.
	internal InternalNetworks
	client   *http.Client
	logger   *slog.Logger
	loader   []byte
	// loaderPath is what the document head links. Content-hashed, so a changed
	// domain or endpoint cannot be served from a year-old cache entry.
	loaderPath string
}

// NewAnalytics wires Plausible for a deployment that asked for it. An empty
// host means no analytics at all and is a supported state: local development
// and the test suite both run that way, and nothing else in the application
// changes.
//
// A configured host with no PUBLIC_ORIGIN is refused rather than started.
// Plausible identifies a site by domain name, and with no origin configured
// there is nothing truthful to send: every event would carry an empty domain,
// be answered 202, and be discarded before it reached storage. Refusing at
// startup costs a restart; the alternative costs however long it takes somebody
// to notice a dashboard that stays at zero.
func NewAnalytics(
	host string,
	publicOrigin string,
	internal InternalNetworks,
	logger *slog.Logger,
) (*Analytics, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return nil, nil
	}
	if err := ValidPublicOrigin(host); err != nil {
		return nil, fmt.Errorf("PLAUSIBLE_HOST: %w", err)
	}
	publicOrigin = strings.TrimSpace(publicOrigin)
	if publicOrigin == "" {
		return nil, fmt.Errorf(
			"PLAUSIBLE_HOST is set but PUBLIC_ORIGIN is not: Plausible identifies a site by its domain, " +
				"and an event carrying none is answered 202 and then discarded")
	}
	if err := ValidPublicOrigin(publicOrigin); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(publicOrigin)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	analytics := &Analytics{
		eventURL: strings.TrimSuffix(host, "/") + AnalyticsEventPath,
		domain:   parsed.Hostname(),
		internal: internal,
		client:   &http.Client{Timeout: analyticsTimeout},
		logger:   logger,
	}
	analytics.loader = buildAnalyticsLoader(analyticsScriptPath, analytics.domain, AnalyticsEventPath)
	analytics.loaderPath = fmt.Sprintf("/js/s.%x.js", sha256.Sum224(analytics.loader))
	return analytics, nil
}

// ScriptPath is the URL the document head links, or "" when analytics are off.
func (a *Analytics) ScriptPath() string {
	if a == nil {
		return ""
	}
	return a.loaderPath
}

// buildAnalyticsLoader writes the one thing the vendored script cannot be told
// any other way. It reads data-domain and data-api off its own <script> element,
// and the framework's view layer emits script tags as bare src attributes -- by
// design, since an application-supplied attribute string would be a hole
// straight through the escaping it does everywhere else. So a same-origin
// loader creates the element instead. default-src 'self' permits a script this
// document injected from its own origin; it would not permit the inline snippet
// that is the usual way to do this.
func buildAnalyticsLoader(scriptPath, domain, eventPath string) []byte {
	quote := func(value string) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			// Marshalling a string cannot fail. Panicking beats emitting
			// JavaScript that parses as something else.
			panic(err)
		}
		return string(encoded)
	}
	return fmt.Appendf(nil, `// Loads the analytics script from this origin, with the site name and the
// event endpoint it cannot read off a bare <script src> tag.
(function () {
  var script = document.createElement("script");
  script.src = %s;
  script.defer = true;
  script.setAttribute("data-domain", %s);
  script.setAttribute("data-api", %s);
  document.head.appendChild(script);
})();
`, quote(scriptPath), quote(domain), quote(eventPath))
}

// Mount registers the three routes on the outermost mux, deliberately outside
// the application router.
//
// The event route has to be: it is a POST, and the router's CSRF middleware
// refuses every unsafe method whose Origin header does not match the scheme
// this process sees. Behind a TLS-terminating proxy that scheme is http while
// the browser sends https, so every posted event would be answered 403 in
// production and pass in local development. The two script routes follow it out
// for a smaller reason -- they are assets, and the traffic recorder should no
// more count a script fetch than it counts the favicon.
func (a *Analytics) Mount(mux *http.ServeMux) {
	if a == nil {
		return
	}
	mux.HandleFunc("GET "+a.loaderPath, a.serveLoader)
	mux.HandleFunc("GET "+analyticsScriptPath, serveAnalyticsScript)
	mux.HandleFunc("POST "+AnalyticsEventPath, a.forwardEvent)
}

func (a *Analytics) serveLoader(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(response, request, "s.js", time.Time{}, bytes.NewReader(a.loader))
}

func serveAnalyticsScript(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(response, request, "p.js", time.Time{}, bytes.NewReader(plausibleJS))
}

// forwardEvent relays one event to Plausible from this server.
//
// The visitor's address travels in X-Plausible-IP, and that header is the whole
// reason this function is careful rather than a two-line reverse proxy.
// Plausible resolves the visitor's address by reading, in order, X-Plausible-IP,
// CF-Connecting-IP, and X-Forwarded-For. Every request out of here crosses
// Cloudflare, which overwrites CF-Connecting-IP with *this server's* address,
// and then a Traefik that does not trust the edge and rewrites X-Forwarded-For
// to whatever it sees. Send only X-Forwarded-For and Plausible reports one
// visitor -- us -- for the entire internet, and the dashboard looks plausible
// enough that nobody checks. Measured on 2026-08-04: an event carrying
// X-Forwarded-For: 8.8.8.8 was filed under Japan, and the same event carrying
// X-Plausible-IP: 8.8.8.8 was filed under the United States.
//
// Our own requests are dropped here rather than relayed. The traffic recorder
// excludes them for the reason written on its InternalNetworks type -- the
// first day's numbers were fifteen visitors and every one of them was a deploy
// check -- and a second measurement that made the same mistake would be worse
// than no second measurement.
func (a *Analytics) forwardEvent(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	body, err := io.ReadAll(io.LimitReader(request.Body, analyticsBodyLimit+1))
	if err != nil || len(body) > analyticsBodyLimit {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	address := clientIP(request)
	if a.internal.Contains(address) {
		// Answered, never forwarded. The browser is told the same thing
		// Plausible would have told it, so nothing retries and nothing logs.
		response.WriteHeader(http.StatusAccepted)
		return
	}
	upstream, err := http.NewRequestWithContext(
		request.Context(), http.MethodPost, a.eventURL, bytes.NewReader(body))
	if err != nil {
		a.fail(response, "building the upstream request", err)
		return
	}
	contentType := request.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}
	upstream.Header.Set("Content-Type", contentType)
	// Plausible reads the user agent to tell a browser from a robot and to fill
	// the device and browser breakdowns. Ours would make every visitor a Go
	// program, which Plausible files as a bot and discards.
	upstream.Header.Set("User-Agent", request.UserAgent())
	upstream.Header.Set("X-Plausible-IP", address)
	upstream.Header.Set("X-Forwarded-For", address)
	relayed, err := a.client.Do(upstream)
	if err != nil {
		a.fail(response, "relaying an analytics event", err)
		return
	}
	defer relayed.Body.Close()
	response.WriteHeader(relayed.StatusCode)
	_, _ = io.Copy(response, io.LimitReader(relayed.Body, analyticsBodyLimit))
}

// fail logs and answers 202. The browser can do nothing useful with a failure
// here and the visitor must never see one: a measurement that breaks the thing
// it measures is worse than a gap in the numbers, which is the same trade the
// traffic recorder makes when it drops events rather than making a request
// wait.
func (a *Analytics) fail(response http.ResponseWriter, doing string, err error) {
	a.logger.Warn("stillworks analytics", "doing", doing, "error", err)
	response.WriteHeader(http.StatusAccepted)
}
