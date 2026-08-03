package audit

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/bon5co/godjango/database"
)

// ShelfRow is one endpoint as the shelf presents it. Every field that makes a
// claim carries the time it was measured; a status without its timestamp is the
// thing this project exists to replace.
type ShelfRow struct {
	Slug          string     `bun:"slug" json:"slug"`
	Provider      string     `bun:"provider" json:"provider"`
	BaseURL       string     `bun:"base_url" json:"base_url"`
	ChatPath      string     `bun:"chat_path" json:"chat_path"`
	DocsURL       string     `bun:"docs_url" json:"docs_url"`
	Notes         string     `bun:"notes" json:"notes,omitempty"`
	Outcome       string     `bun:"outcome" json:"last_outcome"`
	CheckedAt     *time.Time `bun:"checked_at" json:"checked_at"`
	LatencyMS     int        `bun:"latency_ms" json:"latency_ms"`
	ModelsListed  int        `bun:"models_listed" json:"models_listed"`
	WorkingModels int        `bun:"working_models" json:"working_models"`
	KeyOnlyModels int        `bun:"key_only_models" json:"key_only_models"`
}

// WorkingModel is a model verified to answer with no key at all.
type WorkingModel struct {
	Slug             string     `bun:"slug" json:"endpoint"`
	BaseURL          string     `bun:"base_url" json:"base_url"`
	ChatPath         string     `bun:"chat_path" json:"chat_path"`
	ModelID          string     `bun:"model_id" json:"model"`
	Tier             string     `bun:"tier" json:"tier,omitempty"`
	OpenAICompatible bool       `bun:"openai_compatible" json:"openai_compatible"`
	LastOK           *time.Time `bun:"last_ok" json:"last_ok"`
	LatencyMS        int        `bun:"latency_ms" json:"latency_ms"`
	// Attempts and Successes cover the last 7 days. A single green tick hides
	// the thing that actually matters about free infrastructure: pollinations
	// answered 200, then 402 "budget too low", then 200 again inside two
	// minutes on 2026-08-03. "Worked 8 of the last 10 times" is the honest
	// claim; "up" is not.
	Attempts  int `bun:"attempts" json:"recent_attempts"`
	Successes int `bun:"successes" json:"recent_successes"`
}

// OpenAIBaseURL is the value to hand an OpenAI client as its base URL: the
// prefix such that the client's own "/chat/completions" lands on a real route.
//
// Verified against pollinations 2026-08-03: POST /openai/chat/completions
// returns 200 and POST /chat/completions returns 404, so publishing the bare
// host as OPENAI_BASE_URL would hand out a snippet that cannot work.
func (m WorkingModel) OpenAIBaseURL() string {
	const suffix = "/chat/completions"
	base := strings.TrimSuffix(m.BaseURL, "/")
	path := m.ChatPath
	if strings.HasSuffix(path, suffix) {
		path = strings.TrimSuffix(path, suffix)
	}
	return base + path
}

// MarshalJSON adds the derived base URL so a consumer never has to work it out.
func (m WorkingModel) MarshalJSON() ([]byte, error) {
	type payload WorkingModel
	return json.Marshal(struct {
		payload
		OpenAIBaseURL string `json:"openai_base_url,omitempty"`
	}{
		payload:       payload(m),
		OpenAIBaseURL: openAIBaseIfCompatible(m),
	})
}

func openAIBaseIfCompatible(m WorkingModel) string {
	if !m.OpenAICompatible {
		return ""
	}
	return m.OpenAIBaseURL()
}

// ModelRow is the per-endpoint model table.
type ModelRow struct {
	ModelID     string     `bun:"model_id" json:"model"`
	Tier        string     `bun:"tier" json:"tier,omitempty"`
	Keyless     *bool      `bun:"keyless" json:"keyless"`
	ChatCapable bool       `bun:"chat_capable" json:"chat_capable"`
	LastChecked *time.Time `bun:"last_checked" json:"last_checked"`
	LastSeen    time.Time  `bun:"last_seen" json:"last_seen"`
	Attempts    int        `bun:"attempts" json:"recent_attempts"`
	Successes   int        `bun:"successes" json:"recent_successes"`
}

// ProbeRow is one raw recorded attempt, shown so a visitor can check our
// working rather than take it on trust.
type ProbeRow struct {
	StartedAt  time.Time `bun:"started_at" json:"at"`
	Kind       string    `bun:"kind" json:"kind"`
	Outcome    string    `bun:"outcome" json:"outcome"`
	HTTPStatus int       `bun:"http_status" json:"http_status"`
	LatencyMS  int       `bun:"latency_ms" json:"latency_ms"`
	ModelUsed  string    `bun:"model_used" json:"model,omitempty"`
	Error      string    `bun:"error" json:"error,omitempty"`
}

// recentChatReliability counts how often each model actually answered over the
// last week. Rate-limited and budget-exhausted replies count as attempts that
// did not work, because from a caller's seat that is exactly what they are.
const recentChatReliability = `
	SELECT endpoint_id, model_used,
	       count(*)                             AS attempts,
	       count(*) FILTER (WHERE outcome='ok')  AS successes
	FROM llm_probes
	WHERE kind = 'chat' AND started_at > now() - interval '7 days'
	GROUP BY endpoint_id, model_used
`

const latestModelsProbe = `
	SELECT DISTINCT ON (endpoint_id) endpoint_id, outcome, started_at, latency_ms, models_listed
	FROM llm_probes
	WHERE kind = 'models'
	ORDER BY endpoint_id, started_at DESC
`

// Shelf returns every active endpoint with its most recent models probe and a
// count of models proven keyless.
func Shelf(ctx context.Context, db *database.DB) ([]ShelfRow, error) {
	var rows []ShelfRow
	err := db.Bun().NewRaw(`
		SELECT e.slug, e.provider, e.base_url, e.chat_path, e.docs_url, e.notes,
		       COALESCE(p.outcome, 'never_probed') AS outcome,
		       p.started_at AS checked_at,
		       COALESCE(p.latency_ms, 0) AS latency_ms,
		       COALESCE(p.models_listed, 0) AS models_listed,
		       COALESCE(w.working, 0) AS working_models,
		       COALESCE(w.key_only, 0) AS key_only_models
		FROM llm_endpoints e
		LEFT JOIN (`+latestModelsProbe+`) p ON p.endpoint_id = e.id
		LEFT JOIN (
			SELECT endpoint_id,
			       count(*) FILTER (WHERE keyless IS TRUE)  AS working,
			       count(*) FILTER (WHERE keyless IS FALSE) AS key_only
			FROM llm_models GROUP BY endpoint_id
		) w ON w.endpoint_id = e.id
		WHERE e.active
		ORDER BY COALESCE(w.working, 0) DESC, e.slug
	`).Scan(ctx, &rows)
	return rows, err
}

// WorkingModels lists every model verified keyless, freshest first. This is what
// the runtime API serves: an agent asking "what can I call right now".
func WorkingModels(ctx context.Context, db *database.DB, limit int) ([]WorkingModel, error) {
	var rows []WorkingModel
	err := db.Bun().NewRaw(`
		SELECT e.slug, e.base_url, e.chat_path, e.openai_compatible, m.model_id, m.tier, m.last_ok,
		       COALESCE((
		           SELECT latency_ms FROM llm_probes lp
		           WHERE lp.endpoint_id = e.id AND lp.kind = 'chat'
		             AND lp.model_used = m.model_id AND lp.outcome = 'ok'
		           ORDER BY lp.started_at DESC LIMIT 1
		       ), 0) AS latency_ms,
		       COALESCE(r.attempts, 0)  AS attempts,
		       COALESCE(r.successes, 0) AS successes
		FROM llm_models m
		JOIN llm_endpoints e ON e.id = m.endpoint_id
		LEFT JOIN (`+recentChatReliability+`) r
		       ON r.endpoint_id = m.endpoint_id AND r.model_used = m.model_id
		WHERE m.keyless IS TRUE AND e.active
		ORDER BY m.last_ok DESC NULLS LAST
		LIMIT ?
	`, limit).Scan(ctx, &rows)
	return rows, err
}

// EndpointBySlug returns one shelf row, or a zero row when the slug is unknown.
func EndpointBySlug(ctx context.Context, db *database.DB, slug string) (ShelfRow, bool, error) {
	rows, err := Shelf(ctx, db)
	if err != nil {
		return ShelfRow{}, false, err
	}
	for _, row := range rows {
		if row.Slug == slug {
			return row, true, nil
		}
	}
	return ShelfRow{}, false, nil
}

// ModelsFor lists what an endpoint advertises and what we have verified.
func ModelsFor(ctx context.Context, db *database.DB, slug string) ([]ModelRow, error) {
	var rows []ModelRow
	err := db.Bun().NewRaw(`
		SELECT m.model_id, m.tier, m.keyless, m.chat_capable, m.last_checked, m.last_seen,
		       COALESCE(r.attempts, 0)  AS attempts,
		       COALESCE(r.successes, 0) AS successes
		FROM llm_models m
		JOIN llm_endpoints e ON e.id = m.endpoint_id
		LEFT JOIN (`+recentChatReliability+`) r
		       ON r.endpoint_id = m.endpoint_id AND r.model_used = m.model_id
		WHERE e.slug = ?
		ORDER BY m.keyless DESC NULLS LAST, m.model_id
	`, slug).Scan(ctx, &rows)
	return rows, err
}

// RecentProbes is the raw evidence for one endpoint.
func RecentProbes(ctx context.Context, db *database.DB, slug string, limit int) ([]ProbeRow, error) {
	var rows []ProbeRow
	err := db.Bun().NewRaw(`
		SELECT p.started_at, p.kind, p.outcome, p.http_status, p.latency_ms, p.model_used, p.error
		FROM llm_probes p
		JOIN llm_endpoints e ON e.id = p.endpoint_id
		WHERE e.slug = ?
		ORDER BY p.started_at DESC
		LIMIT ?
	`, slug, limit).Scan(ctx, &rows)
	return rows, err
}
