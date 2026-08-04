package audit

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
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

// stylesheetPath is versioned by content so a deploy cannot serve a stale
// cached sheet, and is linked through RenderOptions rather than inlined: the
// default CSP is default-src 'self' and drops inline styles silently.
var stylesheetPath = fmt.Sprintf("/static/stillworks/app.%x.css", sha256.Sum224(appCSS))

// RoutesWithServices is the framework's hook for an app that needs the
// long-lived database pool rather than only a router.
func (a *App) RoutesWithServices(router chi.Router, services web.RuntimeServices) {
	handlers := &handlers{db: services.Database, recorder: a.recorder}
	track := a.recorder.Track

	router.Get(stylesheetPath, serveStylesheet)

	router.Get("/", track(KindPage, handlers.home))
	router.Get("/llm/", track(KindPage, handlers.shelf))
	router.Get("/llm/{slug}", track(KindPage, handlers.endpoint))
	router.Get("/mcp/", track(KindPage, handlers.mcp))

	router.Get("/api/llm/up", track(KindAPI, handlers.apiUp))
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
}

func (h *handlers) home(response http.ResponseWriter, request *http.Request) {
	working, err := WorkingModels(request.Context(), h.db, apiLimit)
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
	rows, err := Shelf(request.Context(), h.db)
	if err != nil {
		serverError(response, request, err)
		return
	}
	working, err := WorkingModels(request.Context(), h.db, apiLimit)
	if err != nil {
		serverError(response, request, err)
		return
	}
	render(response, request, "Free keyless LLM endpoints — stillworks", ShelfPage(rows, working))
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
	working, err := WorkingModels(request.Context(), h.db, apiLimit)
	if err != nil {
		apiError(response, err)
		return
	}
	if strings.EqualFold(request.URL.Query().Get("format"), "env") {
		writeEnv(response, working)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"generated_at": time.Now().UTC(),
		"count":        len(working),
		// Stated in the payload so a consumer cannot mistake this for a
		// guarantee about somebody else's free service.
		"disclaimer": "Every entry was verified by a real call carrying no Authorization header, at the time shown, " +
			"from this service's own IP address. Keyless quotas are commonly per-IP: an endpoint verified here can " +
			"still answer 402 or 429 from yours. These are other people's free services and can add a key " +
			"requirement or disappear at any moment.",
		"models": working,
	})
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
		"models":       models,
		"probes":       probes,
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

func firstOpenAICompatible(working []WorkingModel) (WorkingModel, bool) {
	for _, model := range working {
		if model.OpenAICompatible {
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

func render(response http.ResponseWriter, request *http.Request, title string, content templ.Component) {
	_ = view.Render(response, request, view.RenderOptions{
		Title:       title,
		Content:     content,
		Stylesheets: []string{stylesheetPath},
		CSRFToken:   web.CSRFToken(request),
		PushURL:     request.URL.RequestURI(),
		CachePolicy: view.NoStore,
	})
}

func serverError(response http.ResponseWriter, request *http.Request, err error) {
	response.WriteHeader(http.StatusInternalServerError)
	render(response, request, "Error — stillworks", NotFoundPage(err.Error()))
}
