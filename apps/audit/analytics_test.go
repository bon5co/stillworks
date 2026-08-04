package audit

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testOrigin = "https://stillworks.supercapybara.com"

func newTestAnalytics(t *testing.T, upstream string, internal string) *Analytics {
	t.Helper()
	networks := ParseInternalNetworks(internal, slog.Default())
	analytics, err := NewAnalytics(upstream, testOrigin, networks, slog.Default())
	if err != nil {
		t.Fatalf("NewAnalytics: %v", err)
	}
	if analytics == nil {
		t.Fatal("NewAnalytics returned no analytics for a configured host")
	}
	return analytics
}

// The default is no analytics at all, and it has to stay the default: local
// development and every test in this package run without a Plausible instance,
// and a nil Analytics has to be usable rather than a panic waiting for the
// first request.
func TestAnalyticsAreOffUntilAHostIsNamed(t *testing.T) {
	analytics, err := NewAnalytics("", testOrigin, InternalNetworks{}, slog.Default())
	if err != nil {
		t.Fatalf("an unset PLAUSIBLE_HOST must not be an error: %v", err)
	}
	if analytics != nil {
		t.Fatal("an unset PLAUSIBLE_HOST produced an analytics integration")
	}
	if analytics.ScriptPath() != "" {
		t.Fatal("a nil Analytics named a script path")
	}
	// Mount on nil must be a no-op rather than a panic: main calls it
	// unconditionally.
	analytics.Mount(http.NewServeMux())
}

// A host with no origin is refused at startup. Plausible files an event under
// the domain the event names; one naming nothing is answered 202 and dropped
// before storage, so the deployment would look configured and report nothing.
func TestAnalyticsRefuseAHostWithNothingToCallTheSite(t *testing.T) {
	if _, err := NewAnalytics(
		"https://plausible.example", "", InternalNetworks{}, slog.Default()); err == nil {
		t.Fatal("PLAUSIBLE_HOST without PUBLIC_ORIGIN was accepted")
	}
	for _, host := range []string{
		"https://plausible.example/",
		"plausible.example",
		"https://plausible.example/api",
	} {
		if _, err := NewAnalytics(host, testOrigin, InternalNetworks{}, slog.Default()); err == nil {
			t.Errorf("PLAUSIBLE_HOST %q was accepted", host)
		}
	}
}

// The vendored script reads the site name and the endpoint off its own element,
// and the framework's view layer emits a bare src. The loader is the only place
// those two values can be attached, so its content is worth asserting rather
// than eyeballing.
func TestLoaderCarriesTheSiteNameAndAFirstPartyEndpoint(t *testing.T) {
	analytics := newTestAnalytics(t, "https://plausible.example", "")

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, analytics.ScriptPath(), nil)
	analytics.serveLoader(response, request)

	body := response.Body.String()
	for _, want := range []string{
		`"data-domain", "stillworks.supercapybara.com"`,
		`"data-api", "/api/event"`,
		analyticsScriptPath,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the loader does not carry %q:\n%s", want, body)
		}
	}
	// Not the upstream host: a page that fetched the script or posted events
	// across origins would be stopped by default-src 'self' without saying so.
	if strings.Contains(body, "plausible.example") {
		t.Errorf("the loader names the upstream host, which the CSP forbids reaching:\n%s", body)
	}
	if !strings.Contains(analytics.ScriptPath(), "/js/s.") {
		t.Errorf("loader path %q is not the short neutral name", analytics.ScriptPath())
	}
	if strings.Contains(analytics.ScriptPath()+analyticsScriptPath, "plausible") {
		t.Error("an asset path names the analytics vendor, which is what proxying was for")
	}
}

// Our own requests are dropped here, not relayed. Adding a second measurement
// that counts deploy checks as visitors would repeat the exact mistake the
// traffic recorder's InternalNetworks exists to correct.
func TestOurOwnTrafficIsNeverForwarded(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer upstream.Close()
	analytics := newTestAnalytics(t, upstream.URL, "106.73.62.0, 10.0.0.0/8")

	for _, address := range []string{"106.73.62.0", "10.4.5.6"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, AnalyticsEventPath, strings.NewReader(`{"n":"pageview"}`))
		request.Header.Set("CF-Connecting-IP", address)
		analytics.forwardEvent(response, request)

		if reached {
			t.Fatalf("an event from our own address %s reached Plausible", address)
		}
		// The browser is told what Plausible would have told it, so nothing
		// retries and nothing appears in a console.
		if response.Code != http.StatusAccepted {
			t.Errorf("dropping our own event answered %d, want 202", response.Code)
		}
	}
}

// The visitor's address travels in X-Plausible-IP. X-Forwarded-For alone is not
// enough: Cloudflare overwrites CF-Connecting-IP with this server's address on
// the way out and Plausible prefers that header, so every visitor on earth
// would be filed as one -- us.
func TestAVisitorsAddressAndAgentReachPlausible(t *testing.T) {
	var (
		gotIP        string
		gotForwarded string
		gotAgent     string
		gotType      string
		gotBody      string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		gotIP = request.Header.Get("X-Plausible-IP")
		gotForwarded = request.Header.Get("X-Forwarded-For")
		gotAgent = request.Header.Get("User-Agent")
		gotType = request.Header.Get("Content-Type")
		body, _ := io.ReadAll(request.Body)
		gotBody = string(body)
		response.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(response, "ok")
	}))
	defer upstream.Close()
	analytics := newTestAnalytics(t, upstream.URL, "106.73.62.0")

	const payload = `{"n":"pageview","u":"https://stillworks.supercapybara.com/llm/","d":"stillworks.supercapybara.com"}`
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, AnalyticsEventPath, strings.NewReader(payload))
	request.Header.Set("CF-Connecting-IP", "203.0.113.9")
	// What Traefik rewrote it to. Reading this instead is how every visitor
	// from one Cloudflare edge became a single person once already.
	request.Header.Set("X-Forwarded-For", "172.18.0.4")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Chrome/141.0.0.0")
	request.Header.Set("Content-Type", "text/plain")
	analytics.forwardEvent(response, request)

	if gotIP != "203.0.113.9" {
		t.Errorf("X-Plausible-IP = %q, want the visitor's address", gotIP)
	}
	if gotForwarded != "203.0.113.9" {
		t.Errorf("X-Forwarded-For = %q, want the visitor's address", gotForwarded)
	}
	if gotAgent != "Mozilla/5.0 (Macintosh) Chrome/141.0.0.0" {
		t.Errorf("User-Agent = %q; ours would be filed as a bot and discarded", gotAgent)
	}
	if gotType != "text/plain" {
		t.Errorf("Content-Type = %q, want the browser's own", gotType)
	}
	if gotBody != payload {
		t.Errorf("body = %q, want it relayed untouched", gotBody)
	}
	if response.Code != http.StatusAccepted {
		t.Errorf("answered %d, want the upstream's 202", response.Code)
	}
	if got := response.Body.String(); got != "ok" {
		t.Errorf("body = %q, want the upstream's answer", got)
	}
}

// An upstream that is down, slow or wrong must never become a visitor's
// problem. The event is lost and logged; the page is not.
func TestAnUpstreamFailureIsNotTheVisitorsProblem(t *testing.T) {
	analytics := newTestAnalytics(t, "http://127.0.0.1:1", "")

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, AnalyticsEventPath, strings.NewReader(`{"n":"pageview"}`))
	request.Header.Set("CF-Connecting-IP", "203.0.113.9")
	analytics.forwardEvent(response, request)

	if response.Code != http.StatusAccepted {
		t.Errorf("an unreachable Plausible answered the browser %d, want 202", response.Code)
	}
}

// A body far larger than a pageview is refused rather than relayed.
func TestAnAbsurdBodyIsRefused(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer upstream.Close()
	analytics := newTestAnalytics(t, upstream.URL, "")

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, AnalyticsEventPath,
		strings.NewReader(strings.Repeat("x", analyticsBodyLimit+1)))
	request.Header.Set("CF-Connecting-IP", "203.0.113.9")
	analytics.forwardEvent(response, request)

	if reached {
		t.Error("an oversized body was relayed to Plausible")
	}
	if response.Code != http.StatusBadRequest {
		t.Errorf("answered %d, want 400", response.Code)
	}
}

// Mount puts the event route where no CSRF middleware can reach it, and the
// scripts beside it. A regression here is a 403 that only happens in
// production, because the check compares the Origin header against the scheme
// this process sees -- plaintext behind the proxy, https in the browser.
func TestMountRegistersTheRoutesOutsideTheRouter(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	analytics := newTestAnalytics(t, upstream.URL, "")

	mux := http.NewServeMux()
	analytics.Mount(mux)

	for _, route := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, analytics.ScriptPath(), http.StatusOK},
		{http.MethodGet, analyticsScriptPath, http.StatusOK},
		{http.MethodPost, AnalyticsEventPath, http.StatusAccepted},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(route.method, route.path, strings.NewReader(`{"n":"pageview"}`))
		request.Header.Set("CF-Connecting-IP", "203.0.113.9")
		mux.ServeHTTP(response, request)
		if response.Code != route.want {
			t.Errorf("%s %s answered %d, want %d", route.method, route.path, response.Code, route.want)
		}
	}
}

// The head links the analytics loader only where a deployment configured one,
// and it links it from this origin. A third-party src would be dropped by
// default-src 'self' with nothing on screen and nothing in the logs.
func TestTheHeadLinksAnalyticsOnlyWhenConfigured(t *testing.T) {
	previousOrigin := publicOrigin
	previousScript := analyticsScript
	defer func() {
		SetPublicOrigin(previousOrigin)
		SetAnalyticsScript(previousScript)
	}()
	SetPublicOrigin(testOrigin)

	SetAnalyticsScript("")
	if body := renderShelfWithHost(t, "stillworks.supercapybara.com"); strings.Contains(body, "/js/s.") {
		t.Fatalf("an unconfigured deployment linked an analytics script:\n%s", head(body))
	}

	SetAnalyticsScript("/js/s.deadbeef.js")
	body := renderShelfWithHost(t, "stillworks.supercapybara.com")
	if !strings.Contains(body, `src="/js/s.deadbeef.js"`) {
		t.Fatalf("the configured analytics loader is not in the head:\n%s", head(body))
	}
	// The site's own script keeps its place; analytics is an addition.
	if !strings.Contains(body, scriptPath) {
		t.Fatalf("the site's own script left the head:\n%s", head(body))
	}
}
