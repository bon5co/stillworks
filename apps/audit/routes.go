package audit

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

// Both asset paths are versioned by content so a deploy cannot serve a stale
// cached file, and both are linked through RenderOptions rather than inlined:
// the default CSP is default-src 'self' and drops inline styles and inline
// scripts silently.
var (
	stylesheetPath = fmt.Sprintf("/static/stillworks/app.%x.css", sha256.Sum224(appCSS))
	scriptPath     = fmt.Sprintf("/static/stillworks/app.%x.js", sha256.Sum224(appJS))
)

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

	router.Get("/", track(KindPage, handlers.home))
	router.Get("/llm/", track(KindPage, handlers.shelf))
	// Registered before the slug route so an endpoint can never be named "try".
	router.Get("/llm/try", track(KindPage, handlers.tryCall))
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
	working, err := WorkingModels(request.Context(), h.db, apiLimit, nil)
	if err != nil {
		serverError(response, request, err)
		return
	}
	var checked *time.Time
	if len(working) > 0 {
		checked = working[0].LastOK
	}
	render(response, request, "stillworks — directories list, we check", HomePage(working, checked))
}

func (h *handlers) shelf(response http.ResponseWriter, request *http.Request) {
	query := ParseShelfQuery(request.URL.Query())
	rows, err := ShelfMatching(request.Context(), h.db, query)
	if err != nil {
		serverError(response, request, err)
		return
	}
	working, err := WorkingModelsMatching(request.Context(), h.db, apiLimit, query, nil)
	if err != nil {
		serverError(response, request, err)
		return
	}
	// Models that draw rather than chat, under the same filter. They are a
	// separate table because they are a separate product: an OPENAI_MODEL line
	// naming an image model is a snippet that cannot work.
	drawing, err := WorkingModelsMatching(
		request.Context(), h.db, apiLimit, query, []string{CapabilityImageOut})
	if err != nil {
		serverError(response, request, err)
		return
	}
	// The filter control lists every endpoint, not the filtered set: a dropdown
	// offering only what is already selected cannot be used to change anything.
	slugs, err := EndpointSlugs(request.Context(), h.db)
	if err != nil {
		serverError(response, request, err)
		return
	}
	render(response, request, "Free keyless LLM endpoints — stillworks",
		ShelfPage(rows, working, drawing, query, slugs))
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
	render(response, request, "Test call — stillworks", TryPage(result))
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
		render(response, request, "Not found — stillworks",
			NotFoundPage(fmt.Sprintf("No endpoint called %q is tracked here.", slug)))
		return
	}
	models, err := ModelsFor(request.Context(), h.db, slug)
	if err != nil {
		serverError(response, request, err)
		return
	}
	probes, err := RecentProbes(request.Context(), h.db, slug, 40)
	if err != nil {
		serverError(response, request, err)
		return
	}
	render(response, request, slug+" — stillworks", EndpointPage(row, models, probes))
}

func (h *handlers) mcp(response http.ResponseWriter, request *http.Request) {
	render(response, request, "MCP servers — stillworks", ComingSoonPage())
}

// apiUp is the point of the whole project: an agent asks what it can call right
// now and gets something usable without reading a page.
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
	working, err := WorkingModels(request.Context(), h.db, apiLimit, features)
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
		// guarantee about somebody else's free service.
		"disclaimer": "Every entry was verified by a real call carrying no Authorization header, at the time shown, " +
			"from this service's own IP address. Keyless quotas are commonly per-IP: an endpoint verified here can " +
			"still answer 402 or 429 from yours. These are other people's free services and can add a key " +
			"requirement or disappear at any moment.",
		"models": working,
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
	models, err := ModelsFor(request.Context(), h.db, slug)
	if err != nil {
		apiError(response, err)
		return
	}
	probes, err := RecentProbes(request.Context(), h.db, slug, 40)
	if err != nil {
		apiError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"generated_at": time.Now().UTC(),
		"endpoint":     row,
		// Each model carries its capability record: what the provider claims,
		// what a real call proved, and when each was last established.
		"models":           models,
		"probes":           probes,
		"capability_names": Capabilities,
	})
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
	render(response, request, "Traffic — stillworks", StatsPage(report, h.recorder.Dropped()))
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

func writeEnv(response http.ResponseWriter, working []WorkingModel) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	first, ok := firstOpenAICompatible(working)
	if !ok {
		// Saying nothing is correct here. An Ollama-shaped endpoint cannot be
		// described with OPENAI_ variables, and emitting them anyway would hand
		// out a snippet that fails on first use.
		fmt.Fprintln(response, "# stillworks: no OpenAI-compatible keyless endpoint is currently verified")
		return
	}
	fmt.Fprintf(response, "# stillworks: verified keyless %s\n", humanWhenPtr(first.LastOK))
	fmt.Fprintf(response, "OPENAI_BASE_URL=%s\n", first.OpenAIBaseURL())
	fmt.Fprintln(response, "OPENAI_API_KEY=not-needed")
	fmt.Fprintf(response, "OPENAI_MODEL=%s\n", first.ModelID)
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

func render(response http.ResponseWriter, request *http.Request, title string, content templ.Component) {
	_ = view.Render(response, request, view.RenderOptions{
		Title:       title,
		Content:     content,
		Stylesheets: []string{stylesheetPath},
		// The script only ever improves on a page that already works without
		// it: sorting, filtering and the test call are all server routes first.
		Scripts:     []string{scriptPath},
		CSRFToken:   web.CSRFToken(request),
		PushURL:     request.URL.RequestURI(),
		CachePolicy: view.NoStore,
	})
}

func serverError(response http.ResponseWriter, request *http.Request, err error) {
	response.WriteHeader(http.StatusInternalServerError)
	render(response, request, "Error — stillworks", NotFoundPage(err.Error()))
}
