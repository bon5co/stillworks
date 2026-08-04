package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/bon5co/godjango/database"
	"github.com/bon5co/godjango/web"
	"github.com/bon5co/godjango/web/view"
	"github.com/go-chi/chi/v5"
)

// apiLimit caps how many verified models the runtime API returns per call.
const apiLimit = 50

// The two shelves have two addresses rather than one address with a filter.
//
// A filter has a default, and the default is what everybody sees and links to.
// Making "which claim is this" a query parameter means one careless glance --
// or one link that lost its parameters -- reads a key-required endpoint as
// keyless, which is the exact confusion this project was built to attack. A
// path cannot be lost: /llm/ pasted into a chat window is the keyless shelf and
// nothing else, today and after any future default changes.
//
// The cost is one more page. It is small: both are the same component with the
// auth mode threaded through, and every query underneath already takes the mode
// as an argument.
const (
	keylessShelfPath = "/llm/"
	keyedShelfPath   = "/llm/keyed/"
)

// statsDays is the window the traffic page charts; statsLimit caps the
// referrer and path tables. Totals are always all-time -- the outcome this
// project is measured against is cumulative, so a rolling headline number
// would flatter it.
const (
	statsDays  = 14
	statsLimit = 10
)

//go:embed static/app.css
var appCSS []byte

//go:embed static/app.js
var appJS []byte

// The mark: one verified-green sample square, the same one that sits at the end
// of the pulse on the social card. Repetition is the whole value of a
// distinctive asset, so the two files are deliberately the same shape.
//
//go:embed static/favicon.png
var faviconPNG []byte

// The social card, linked from every page's head since godjango grew
// RenderOptions.Meta. Before that it was embedded and served and pointed at by
// nothing, so a link to this site pasted into a chat window rendered as a blank
// rectangle while the finished image sat on disk one URL away.
//
//go:embed static/og-card.png
var socialCardPNG []byte

// Both asset paths are versioned by content so a deploy cannot serve a stale
// cached file, and both are linked through RenderOptions rather than inlined:
// the default CSP is default-src 'self' and drops inline styles and inline
// scripts silently.
var (
	stylesheetPath = fmt.Sprintf("/static/stillworks/app.%x.css", sha256.Sum224(appCSS))
	scriptPath     = fmt.Sprintf("/static/stillworks/app.%x.js", sha256.Sum224(appJS))
	faviconPath    = fmt.Sprintf("/static/stillworks/mark.%x.png", sha256.Sum224(faviconPNG))
	// SocialCardPath is exported because a link preview needs an absolute URL,
	// which only the deployment knows.
	socialCardPath = fmt.Sprintf("/static/stillworks/card.%x.png", sha256.Sum224(socialCardPNG))
)

// SocialCardPath is the content-hashed path of the link-preview image, for
// whoever ends up writing the og:image tag.
func SocialCardPath() string { return socialCardPath }

// RoutesWithServices is the framework's hook for an app that needs the
// long-lived database pool rather than only a router.
func (a *App) RoutesWithServices(router chi.Router, services web.RuntimeServices) {
	handlers := &handlers{
		db:       services.Database,
		recorder: a.recorder,
		tries:    NewTryRunner(a.recorder),
	}
	track := a.recorder.Track

	router.Get(stylesheetPath, serveStylesheet)
	router.Get(scriptPath, serveScript)

	// /favicon.ico is requested by every browser without being linked, which is
	// the only reason the tab icon can ship before the framework can express a
	// <link> tag at all. Neither asset is counted: a browser fetching an icon
	// is not a visit, and inflating the number the project is judged on with
	// automatic requests would make it worthless.
	router.Get("/favicon.ico", serveFavicon)
	router.Get(faviconPath, serveFavicon)
	router.Get(socialCardPath, serveSocialCard)

	router.Get("/", track(KindPage, handlers.home))
	router.Get(keylessShelfPath, track(KindPage, handlers.shelf))
	// Registered before the slug route so an endpoint can never be named "try"
	// or "keyed". Both spellings of the keyed shelf are registered because
	// /llm/keyed with no trailing slash would otherwise be matched by the slug
	// route and 404 as an unknown endpoint.
	router.Get("/llm/try", track(KindPage, handlers.tryCall))
	router.Get(keyedShelfPath, track(KindPage, handlers.keyedShelf))
	router.Get(strings.TrimSuffix(keyedShelfPath, "/"), track(KindPage, handlers.keyedShelf))
	router.Get("/llm/{slug}", track(KindPage, handlers.endpoint))
	router.Get("/mcp/", track(KindPage, handlers.mcp))

	router.Get("/api/llm/up", track(KindAPI, handlers.apiUp))
	router.Get("/api/llm/try", track(KindAPI, handlers.apiTryCall))
	router.Get("/api/llm/{slug}", track(KindAPI, handlers.apiEndpoint))

	// The two routes that report the numbers are the two routes that are never
	// counted. Reading a scoreboard must not move it, and an outcome contract
	// checked by a page that inflates itself is not an outcome contract.
	router.Get("/stats", handlers.stats)
	router.Get("/api/stats", handlers.apiStats)
}

type handlers struct {
	db       *database.DB
	recorder *Recorder
	tries    *TryRunner
}

func (h *handlers) home(response http.ResponseWriter, request *http.Request) {
	working, err := WorkingModels(request.Context(), h.db, apiLimit, nil, AuthModeNone)
	if err != nil {
		serverError(response, request, err)
		return
	}
	// The keyed count is fetched separately and shown as its own line. Adding
	// the two together would produce a single headline number that is true of
	// nothing: no caller can use all of them under one set of conditions.
	keyed, err := WorkingModels(request.Context(), h.db, apiLimit, nil, AuthModeKey)
	if err != nil {
		serverError(response, request, err)
		return
	}
	// Both counts carry their own measurement time. A number on this site
	// without a time beside it is the thing the tagline promises never happens.
	render(response, request, page{
		Title: "stillworks — directories list, we check",
		Description: "Free LLM endpoints that need no key and no signup, re-probed by real calls " +
			"and published with the time each one was measured. Nothing here says \"up\".",
		Canonical: "/",
	}, HomePage(working, keyed, lastVerified(working), lastVerified(keyed)))
}

// lastVerified is the most recent verification anywhere in the list, scanned
// rather than read off the head.
//
// It used to return models[0].LastOK, which was true only for as long as the
// list was ordered freshest-first. The shelf now orders by reliability, and the
// head of that ordering is routinely not the newest row -- so the head's
// timestamp would have gone on the home page under the words "last checked" and
// been wrong by hours, which is precisely the failure this site exists to name
// in other people's directories.
func lastVerified(models []WorkingModel) *time.Time {
	var latest *time.Time
	for _, model := range models {
		if model.LastOK == nil {
			continue
		}
		if latest == nil || model.LastOK.After(*latest) {
			latest = model.LastOK
		}
	}
	return latest
}

func (h *handlers) shelf(response http.ResponseWriter, request *http.Request) {
	h.renderShelf(response, request, AuthModeNone)
}

// keyedShelf is the second shelf: providers whose free tier needs a key, probed
// with a key of our own. Its results are a weaker claim than the first shelf's
// and the page says so in every place a number appears.
func (h *handlers) keyedShelf(response http.ResponseWriter, request *http.Request) {
	h.renderShelf(response, request, AuthModeKey)
}

func (h *handlers) renderShelf(response http.ResponseWriter, request *http.Request, auth string) {
	query := ParseShelfQuery(request.URL.Query()).On(shelfPathFor(auth))
	rows, err := ShelfMatching(request.Context(), h.db, query, auth)
	if err != nil {
		serverError(response, request, err)
		return
	}
	// The capability filter reaches the page's own table now. It arrives in the
	// URL the page's own prose tells visitors to write, and for the shelf's
	// first day the HTML parsed it, ignored it, and returned every row -- so
	// ?feature=tools read as "all twenty-three of these do tool calling".
	working, err := WorkingModelsMatching(request.Context(), h.db, apiLimit, query, query.Features, auth)
	if err != nil {
		serverError(response, request, err)
		return
	}
	// Models that draw rather than chat, under the same filter. They are a
	// separate table because they are a separate product: an OPENAI_MODEL line
	// naming an image model is a snippet that cannot work.
	drawing, err := WorkingModelsMatching(
		request.Context(), h.db, apiLimit, query, []string{CapabilityImageOut}, auth)
	if err != nil {
		serverError(response, request, err)
		return
	}
	// The filter control lists every endpoint on this shelf, not the filtered
	// set: a dropdown offering only what is already selected cannot be used to
	// change anything.
	slugs, err := EndpointSlugs(request.Context(), h.db, auth)
	if err != nil {
		serverError(response, request, err)
		return
	}
	// Whether this deployment can reach this shelf's providers at all. An empty
	// keyed shelf on an instance holding no keys is a fact about the instance,
	// and saying "nothing is verified" without that would be a claim about
	// somebody else's service that we have no evidence for.
	configured, err := h.shelfIsConfigured(request.Context(), auth)
	if err != nil {
		serverError(response, request, err)
		return
	}
	render(response, request, page{
		Title:       shelfTitleFor(auth),
		Description: shelfDescriptionFor(auth),
		Canonical:   shelfPathFor(auth),
	}, ShelfPage(rows, working, drawing, query, slugs, auth, configured))
}

// shelfIsConfigured reports whether we hold a credential for at least one
// endpoint on this shelf. The keyless shelf needs none and is always configured.
func (h *handlers) shelfIsConfigured(ctx context.Context, auth string) (bool, error) {
	if auth != AuthModeKey {
		return true, nil
	}
	names, err := KeyEnvsFor(ctx, h.db, auth)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true, nil
		}
	}
	return false, nil
}

func shelfPathFor(auth string) string {
	if auth == AuthModeKey {
		return keyedShelfPath
	}
	return keylessShelfPath
}

func shelfTitleFor(auth string) string {
	if auth == AuthModeKey {
		return "Free-tier LLM endpoints that need a key — stillworks"
	}
	return "Free keyless LLM endpoints — stillworks"
}

func shelfDescriptionFor(auth string) string {
	if auth == AuthModeKey {
		return "Providers whose free tier needs an API key, re-probed with our own free-tier key and " +
			"published with the time each result was measured. Kept apart from the keyless shelf."
	}
	return "LLM endpoints that answer with no API key and no signup, with the base URL, the model id " +
		"and a working curl. Every claim carries when it was measured and which features a real call proved."
}

// tryCall runs one call on a visitor's behalf and renders what happened. It is
// a GET, and the whole request is in the URL, for the same reason the shelf's
// sort is: it has to work with no JavaScript, and the page's own JavaScript
// only ever improves on it by making the call from the visitor's browser
// instead. A POST would additionally need a CSRF token, and godjango's origin
// check compares the Origin header against request.TLS, which is nil behind
// this deployment's TLS-terminating proxy -- so a form POST would be refused in
// production and pass in local development.
//
// The cost of GET is that a link can be followed by something that is not a
// person. That is covered where it matters: crawlers are refused, both rate
// limits still apply, and the set of callable pairs is the set already in the
// database.
func (h *handlers) tryCall(response http.ResponseWriter, request *http.Request) {
	result, err := h.runTryCall(request)
	if err != nil {
		serverError(response, request, err)
		return
	}
	render(response, request, page{Title: "Test call — stillworks"}, TryPage(result))
}

func (h *handlers) apiTryCall(response http.ResponseWriter, request *http.Request) {
	result, err := h.runTryCall(request)
	if err != nil {
		apiError(response, err)
		return
	}
	// A refusal carries the status its reason deserves: an untracked pair will
	// never work and a rate limit will, and an API caller has to be able to tell
	// those apart without reading prose.
	writeJSON(response, result.RefusalStatus(), map[string]any{
		"generated_at": time.Now().UTC(),
		"note": "This call was made from stillworks' own server, not from your address. Keyless quotas are " +
			"commonly per-IP, so the same request from your machine can get a different answer.",
		"result": result,
	})
}

func (h *handlers) runTryCall(request *http.Request) (TryResult, error) {
	values := request.URL.Query()
	// Both are looked up as exact values in the database, so the only thing
	// bounding them buys is refusing to carry an absurd string that far. Model
	// ids run long -- some are a vendor prefix and a version -- so the bound is
	// not the filter text limit.
	return h.tries.Run(
		request.Context(),
		h.db,
		request,
		clip(strings.TrimSpace(values.Get("endpoint")), 64),
		clip(strings.TrimSpace(values.Get("model")), 200),
		values.Get("prompt"),
	)
}

func (h *handlers) endpoint(response http.ResponseWriter, request *http.Request) {
	slug := chi.URLParam(request, "slug")
	row, found, err := EndpointBySlug(request.Context(), h.db, slug)
	if err != nil {
		serverError(response, request, err)
		return
	}
	if !found {
		response.WriteHeader(http.StatusNotFound)
		render(response, request, page{Title: "Not found — stillworks"},
			NotFoundPage(fmt.Sprintf("No endpoint called %q is tracked here.", slug)))
		return
	}
	models, err := ModelsFor(request.Context(), h.db, slug, row.AuthMode)
	if err != nil {
		serverError(response, request, err)
		return
	}
	probes, err := RecentProbes(request.Context(), h.db, slug, 40)
	if err != nil {
		serverError(response, request, err)
		return
	}
	render(response, request, page{
		Title: slug + " — stillworks",
		Description: fmt.Sprintf(
			"%s: the base URL, a working call, every model we have checked and the raw probe log behind each verdict.",
			row.Provider),
		Canonical: "/llm/" + slug,
	}, EndpointPage(row, models, probes))
}

func (h *handlers) mcp(response http.ResponseWriter, request *http.Request) {
	render(response, request, page{
		Title:       "MCP servers — stillworks",
		Description: "The MCP shelf is not open yet. The LLM shelf ships first and has to earn its keep.",
		Canonical:   "/mcp/",
	}, ComingSoonPage())
}

// apiUp is the point of the whole project: an agent asks what it can call right
// now and gets something usable without reading a page.
//
// With no parameters it returns keyless models and nothing else, and it will
// keep doing that. Callers wrote this URL into their code when keyless was the
// only thing on the site; quietly widening it would hand somebody a base URL
// that 401s in production, at a moment they have no reason to be looking. The
// second shelf is reachable only by asking for it.
func (h *handlers) apiUp(response http.ResponseWriter, request *http.Request) {
	features, err := requestedFeatures(request.URL.Query())
	if err != nil {
		// The caller asked for something by name. Answering 200 with a shorter
		// list would look like "nothing supports that", which is a different
		// and much more expensive thing to be told.
		writeJSON(response, http.StatusBadRequest, map[string]any{
			"error":          err.Error(),
			"valid_features": Capabilities,
		})
		return
	}
	auth, err := requestedAuthMode(request.URL.Query())
	if err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]any{
			"error":            err.Error(),
			"valid_auth_modes": AuthModes,
		})
		return
	}
	working, err := WorkingModels(request.Context(), h.db, apiLimit, features, auth)
	if err != nil {
		apiError(response, err)
		return
	}
	if strings.EqualFold(request.URL.Query().Get("format"), "env") {
		writeEnv(response, working)
		return
	}
	payload := map[string]any{
		"generated_at": time.Now().UTC(),
		"count":        len(working),
		// Stated in the payload so a consumer cannot mistake this for a
		// guarantee about somebody else's free service. The keyless wording is
		// left exactly as it was: it is the payload a caller predating the
		// second shelf receives, and it is the stronger of the two claims.
		"disclaimer": disclaimerFor(auth),
		"models":     working,
	}
	if auth != AuthModeNone {
		// Echoed back, and qualified. A caller that opted in has to be told what
		// it opted into, on the envelope as well as on each row.
		payload["auth"] = auth
		payload["auth_disclaimer"] = keyNote + " Every entry carries auth_mode: \"none\" needs no key, " +
			"\"key\" needs one of your own. Requesting auth=none, or omitting the parameter entirely, " +
			"returns keyless entries only."
	}
	if len(features) > 0 {
		// Echoed back so a caller can see its filter was understood rather than
		// silently dropped. The key is absent when nothing was asked for, so
		// the payload a caller predating the filter receives is unchanged.
		payload["features"] = features
		payload["feature_disclaimer"] = "A feature is listed only where a real call proved it: a tool call the " +
			"caller could dispatch, a reply that parsed and matched the schema, an answer about an image we sent. " +
			"Models whose features have never been probed are not returned by a filtered request."
	}
	writeJSON(response, http.StatusOK, payload)
}

func (h *handlers) apiEndpoint(response http.ResponseWriter, request *http.Request) {
	slug := chi.URLParam(request, "slug")
	row, found, err := EndpointBySlug(request.Context(), h.db, slug)
	if err != nil {
		apiError(response, err)
		return
	}
	if !found {
		writeJSON(response, http.StatusNotFound, map[string]any{"error": "unknown endpoint", "slug": slug})
		return
	}
	models, err := ModelsFor(request.Context(), h.db, slug, row.AuthMode)
	if err != nil {
		apiError(response, err)
		return
	}
	probes, err := RecentProbes(request.Context(), h.db, slug, 40)
	if err != nil {
		apiError(response, err)
		return
	}
	payload := map[string]any{
		"generated_at": time.Now().UTC(),
		"endpoint":     row,
		// Each model carries its capability record: what the provider claims,
		// what a real call proved, and when each was last established. It also
		// carries both verdict fields: keyless is the answer to "does this
		// answer with no key", answered_with_key to "does it answer ours". Only
		// the one this endpoint's auth mode asks is ever filled in.
		"models":           models,
		"probes":           probes,
		"capability_names": Capabilities,
	}
	if row.RequiresKey() {
		payload["key_note"] = keyNote
	}
	writeJSON(response, http.StatusOK, payload)
}

// stats publishes this site's own traffic. A directory that tells other people
// how often their endpoints answered, while keeping its own numbers private,
// is asking for a trust it will not extend.
func (h *handlers) stats(response http.ResponseWriter, request *http.Request) {
	report, err := Traffic(request.Context(), h.db, statsDays, statsLimit)
	if err != nil {
		serverError(response, request, err)
		return
	}
	render(response, request, page{
		Title: "Traffic — stillworks",
		Description: "This site's own numbers, published to the standard it asks of everybody else: " +
			"what was counted, what was excluded, and what was lost.",
		Canonical: "/stats",
	}, StatsPage(report, h.recorder.Dropped()))
}

func (h *handlers) apiStats(response http.ResponseWriter, request *http.Request) {
	report, err := Traffic(request.Context(), h.db, statsDays, statsLimit)
	if err != nil {
		apiError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"generated_at": time.Now().UTC(),
		"window_days":  statsDays,
		"note": "Sessions are distinct daily visitors: a salted hash of address and user agent that " +
			"changes every UTC day and is never stored alongside the address itself. Requests to " +
			"/stats and /api/stats are not counted. Crawler hits are recorded and excluded.",
		"dropped_events": h.recorder.Dropped(),
		"traffic":        report,
	})
}

// requestedFeatures reads ?feature=tools,json_schema -- repeatable and
// comma-separated, both, because an agent writing the query string by hand will
// try whichever it thought of first. Multiple features are ANDed: an agent that
// needs tools and a schema needs one model that does both, not two models.
//
// An unknown name is a 400 rather than a filter that quietly matches nothing.
// "No models support tols" is a true sentence that would send somebody looking
// for a provider outage.
func requestedFeatures(query url.Values) ([]string, error) {
	var features []string
	for _, value := range query["feature"] {
		for _, name := range strings.Split(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if !KnownCapability(name) {
				return nil, fmt.Errorf("unknown feature %q", name)
			}
			if !slices.Contains(features, name) {
				features = append(features, name)
			}
		}
	}
	// image_out belongs to models that draw and the others to models that talk,
	// so a request for both can never match anything. Saying so beats an empty
	// list that looks like everybody's endpoint went down.
	if slices.Contains(features, CapabilityImageOut) && len(features) > 1 {
		return nil, fmt.Errorf("%q cannot be combined with a chat feature: no model both draws and chats",
			CapabilityImageOut)
	}
	return features, nil
}

// requestedAuthMode reads ?auth=none|key|any. Absent means none: the parameter
// exists so the second shelf can be asked for, never so it can arrive
// unrequested.
//
// An unknown value is a 400 naming the valid ones, for the same reason an
// unknown feature is. Silently falling back to keyless would be safe but
// baffling -- a caller that typed auth=keyed would get a list with no
// explanation of why its filter did nothing.
func requestedAuthMode(query url.Values) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(query.Get("auth")))
	if raw == "" {
		return AuthModeNone, nil
	}
	if !KnownAuthMode(raw) {
		return "", fmt.Errorf("unknown auth mode %q", raw)
	}
	return raw, nil
}

// disclaimerFor keeps the strongest true sentence on each answer. A caller
// asking for keyless results gets the keyless disclaimer word for word, because
// widening the prose to cover rows it did not ask for would weaken a claim we
// can actually stand behind.
func disclaimerFor(auth string) string {
	const keyless = "Every entry was verified by a real call carrying no Authorization header, at the time shown, " +
		"from this service's own IP address. Keyless quotas are commonly per-IP: an endpoint verified here can " +
		"still answer 402 or 429 from yours. These are other people's free services and can add a key " +
		"requirement or disappear at any moment."
	const withKeys = "Every entry was verified by a real call, at the time shown, from this service's own IP " +
		"address. Entries with auth_mode \"none\" were called with no Authorization header at all; entries " +
		"with auth_mode \"key\" were called with stillworks' own free-tier key for that provider. Quotas are " +
		"commonly per-IP and per-account: an endpoint verified here can still answer 402 or 429 for you. " +
		"These are other people's free services and can change or disappear at any moment."
	if auth == AuthModeNone {
		return keyless
	}
	return withKeys
}

func writeEnv(response http.ResponseWriter, working []WorkingModel) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	first, ok := firstOpenAICompatible(working)
	if !ok {
		// Saying nothing is correct here. An Ollama-shaped endpoint cannot be
		// described with OPENAI_ variables, and emitting them anyway would hand
		// out a snippet that fails on first use.
		fmt.Fprintln(response, "# stillworks: no OpenAI-compatible endpoint is currently verified for that request")
		return
	}
	if first.RequiresKey() {
		// The placeholder is deliberately not a value. Our own key is never
		// rendered anywhere, and a file that looked complete would be pasted
		// into a project and fail at the first call with nothing to explain it.
		fmt.Fprintf(response, "# stillworks: %s requires an API key. Verified with our own free-tier key %s;\n",
			first.Slug, humanWhenPtr(first.LastOK))
		fmt.Fprintln(response, "# that is not a statement about what your signup's free tier includes.")
		fmt.Fprintln(response, EnvLines(first))
		return
	}
	fmt.Fprintf(response, "# stillworks: verified keyless %s\n", humanWhenPtr(first.LastOK))
	fmt.Fprintln(response, EnvLines(first))
}

// EnvLines is the three-line block an OpenAI client is configured with. It is
// one function so the page and the API cannot disagree about what the base URL
// is -- the page printed the raw base_url column instead for the shelf's first
// day, which for OVH is missing the /v1 and answers 404 to everything.
//
// The key line is a placeholder on the keyed shelf and never a value: ours is
// ours, and a block that looked complete would be pasted into a project and
// fail on the first call with nothing on screen to explain why.
func EnvLines(model WorkingModel) string {
	key := "not-needed"
	if model.RequiresKey() {
		key = "your-" + model.Slug + "-api-key"
	}
	return fmt.Sprintf("OPENAI_BASE_URL=%s\nOPENAI_API_KEY=%s\nOPENAI_MODEL=%s",
		model.OpenAIBaseURL(), key, model.ModelID)
}

// firstOpenAICompatible skips models that cannot answer a chat call at all.
// OPENAI_MODEL only means something to a chat client, so handing back an image
// model -- which ?feature=image_out returns -- would produce a .env that fails
// on first use.
func firstOpenAICompatible(working []WorkingModel) (WorkingModel, bool) {
	for _, model := range working {
		if model.OpenAICompatible && model.ChatCapable {
			return model, true
		}
	}
	return WorkingModel{}, false
}

func writeJSON(response http.ResponseWriter, status int, payload any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Agents are the intended caller and they run everywhere.
	response.Header().Set("Access-Control-Allow-Origin", "*")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	encoder := json.NewEncoder(response)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(payload)
}

func apiError(response http.ResponseWriter, err error) {
	writeJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

func serveStylesheet(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/css; charset=utf-8")
	// Safe to cache hard: the path carries the content hash.
	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(response, request, "app.css", time.Time{}, bytes.NewReader(appCSS))
}

func serveScript(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(response, request, "app.js", time.Time{}, bytes.NewReader(appJS))
}

func serveFavicon(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "image/png")
	// /favicon.ico carries no content hash, so it gets a day rather than a
	// year: long enough to stop the repeat fetches, short enough that a changed
	// mark is not stuck in caches for a year.
	if request.URL.Path == faviconPath {
		response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		response.Header().Set("Cache-Control", "public, max-age=86400")
	}
	http.ServeContent(response, request, "favicon.png", time.Time{}, bytes.NewReader(faviconPNG))
}

func serveSocialCard(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "image/png")
	response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(response, request, "og-card.png", time.Time{}, bytes.NewReader(socialCardPNG))
}

// page is one rendered page's own head metadata. Title is required; the rest is
// what a link preview and a search result are built from, and a page that
// declares none of it gets no social tags at all rather than half a card.
type page struct {
	Title string
	// Description is the sentence a search result and a link preview quote. It
	// says what was measured, like everything else here: "verified by real
	// calls" and not "the best free LLM list".
	Description string
	// Canonical is this page's own stable address, without the sort and filter
	// parameters that produce a hundred spellings of the same shelf. Naming the
	// request's own URL instead would ask a crawler to index every one of them.
	Canonical string
}

// publicOrigin is the scheme and host this deployment is reached under, for
// example https://stillworks.supercapybara.com. It is written once at startup
// by SetPublicOrigin and read on every request; nothing writes it afterwards.
//
// Empty is a supported, safe state and not a fallback to the request's own
// Host header. The Host header is the client's own text, so resolving a
// canonical URL or an og:image against it lets a request arriving under any
// other name that routes here write that name into this page's canonical URL --
// which is the ordinary way to hand somebody your search ranking. With no origin
// configured this site publishes no canonical and no social card at all, which
// is exactly what it published before any of this existed.
var publicOrigin = strings.TrimSpace(os.Getenv("PUBLIC_ORIGIN"))

// analyticsScript is the same-origin loader that starts Plausible, or empty on
// a deployment that runs no analytics -- which is every deployment until
// PLAUSIBLE_HOST is set, including local development and the test suite.
// Startup only, and for the same reason publicOrigin is: it is written once
// before the listener opens and read on every request afterwards.
var analyticsScript string

// SetAnalyticsScript hands the view layer the analytics loader's URL. Empty
// means the head links no analytics at all, which is the default.
func SetAnalyticsScript(path string) { analyticsScript = strings.TrimSpace(path) }

// SetPublicOrigin lets the server hand over the configured origin at startup,
// so the environment is read through the project's own typed settings rather
// than a second time from this package. Startup only: it is not safe to call
// once requests are being served.
func SetPublicOrigin(origin string) { publicOrigin = strings.TrimSpace(origin) }

// ValidPublicOrigin reports whether a configured origin is one the view layer
// will accept, so the server can refuse to start rather than discover it one
// request at a time.
//
// This exists because the failure is silent and total. The view layer resolves
// the whole document head before writing a byte and returns an error if it
// cannot; a trailing slash on the origin is enough. With that error discarded --
// which is what this file used to do -- every HTML page answers 200 with an
// empty body while /healthz stays 204 and /api/llm/up keeps serving JSON. The
// deployment looks healthy, the API looks healthy, and every human sees a blank
// white page. A trailing slash is the single most likely thing to be typed into
// a deployment's environment field.
func ValidPublicOrigin(origin string) error {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	switch {
	case err != nil:
		return fmt.Errorf("PUBLIC_ORIGIN %q is not a URL: %w", origin, err)
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return fmt.Errorf("PUBLIC_ORIGIN %q needs an http:// or https:// scheme", origin)
	case parsed.Host == "":
		return fmt.Errorf("PUBLIC_ORIGIN %q names no host", origin)
	case parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "":
		return fmt.Errorf(
			"PUBLIC_ORIGIN %q must be exactly scheme://host[:port] — no path, no query, no trailing slash", origin)
	}
	return nil
}

func render(response http.ResponseWriter, request *http.Request, meta page, content templ.Component) {
	// The site's own script first, analytics after it: nothing on the page
	// waits on either, and the order says which one the visitor is here for.
	// Both are same-origin, which is what default-src 'self' permits and the
	// reason the analytics script is vendored rather than linked.
	scripts := []string{scriptPath}
	if analyticsScript != "" {
		scripts = append(scripts, analyticsScript)
	}
	options := view.RenderOptions{
		Title:       meta.Title,
		Content:     content,
		Stylesheets: []string{stylesheetPath},
		// The script only ever improves on a page that already works without
		// it: sorting, filtering, the copy buttons and the test call are all
		// usable with it switched off.
		Scripts: scripts,
		Meta: view.Meta{
			Description: meta.Description,
			Origin:      publicOrigin,
			// The icon href is relative and stays relative: a browser resolves
			// it against the document, so it needs no origin and is safe on
			// every page including the ones that declare nothing else.
			Icon: view.Icon{
				Href: faviconPath,
				Type: "image/png",
			},
		},
		CSRFToken:   web.CSRFToken(request),
		PushURL:     request.URL.RequestURI(),
		CachePolicy: view.NoStore,
	}
	// A canonical URL and a link preview are absolute, so both need an origin we
	// are willing to stand behind. Without one they are simply not published.
	//
	// They are also only published by a page that named itself: the error page
	// and the 404 pass no description and no canonical, and a shared 404 that
	// previews as the finished product card is a small lie of exactly the kind
	// this site exists to complain about.
	if publicOrigin != "" && meta.Canonical != "" && meta.Description != "" {
		options.Meta.Canonical = meta.Canonical
		options.Meta.Social = view.Social{
			Image: socialCardPath,
			ImageAlt: "stillworks: a row of sample squares, most of them verified green, " +
				"one refused, over the words \"directories list, we check\".",
			SiteName: "stillworks",
		}
	}
	if err := view.Render(response, request, options); err != nil {
		// Never silently. The render fails before writing anything, so swallowing
		// it serves a 200 with no body -- indistinguishable from a working page
		// to every health check and invisible in the logs.
		slog.Error("stillworks: rendering a page failed", "path", request.URL.Path, "error", err)
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		response.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(response, "stillworks: this page could not be rendered. The failure is in our logs.")
	}
}

func serverError(response http.ResponseWriter, request *http.Request, err error) {
	response.WriteHeader(http.StatusInternalServerError)
	render(response, request, page{Title: "Error — stillworks"}, NotFoundPage(err.Error()))
}
