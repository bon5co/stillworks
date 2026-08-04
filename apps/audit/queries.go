package audit

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/bon5co/godjango/database"
	"github.com/uptrace/bun"
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

// CapabilityRecord is one published feature verdict, with the evidence that
// produced it. Claimed and Supported are separate fields because they disagree:
// llm7 claims json_mode for meta-Llama-3.1-8B-Instruct-Turbo and answers 405 to
// the json_schema request, and a shelf that showed only one number would have to
// pick which of those to be wrong about.
type CapabilityRecord struct {
	Capability  string     `bun:"capability" json:"-"`
	ModelRowID  int64      `bun:"model_id" json:"-"`
	Claimed     *bool      `bun:"claimed" json:"claimed"`
	Supported   *bool      `bun:"supported" json:"supported"`
	LastChecked *time.Time `bun:"last_checked" json:"checked_at"`
	LastOK      *time.Time `bun:"last_ok" json:"last_ok"`
	LastError   string     `bun:"last_error" json:"last_error,omitempty"`
}

// Verified reports whether a real call proved this feature works.
func (record CapabilityRecord) Verified() bool {
	return record.Supported != nil && *record.Supported
}

// ClaimContradicted reports a provider that publishes a capability we measured
// as absent. This is the reason the shelf exists, so it gets its own name.
func (record CapabilityRecord) ClaimContradicted() bool {
	return record.Claimed != nil && *record.Claimed &&
		record.Supported != nil && !*record.Supported
}

// WorkingModel is a model verified to answer with no key at all.
type WorkingModel struct {
	Slug             string     `bun:"slug" json:"endpoint"`
	BaseURL          string     `bun:"base_url" json:"base_url"`
	ChatPath         string     `bun:"chat_path" json:"chat_path"`
	ModelRowID       int64      `bun:"model_row_id" json:"-"`
	ModelID          string     `bun:"model_id" json:"model"`
	Tier             string     `bun:"tier" json:"tier,omitempty"`
	ChatCapable      bool       `bun:"chat_capable" json:"chat_capable"`
	OpenAICompatible bool       `bun:"openai_compatible" json:"openai_compatible"`
	LastOK           *time.Time `bun:"last_ok" json:"last_ok"`
	LatencyMS        int        `bun:"latency_ms" json:"latency_ms"`
	// Capabilities carries one entry per feature we have a record for, keyed by
	// capability name. A feature that is absent from the map has never been
	// asked, which is not the same as a "no" and is never rendered as one.
	Capabilities map[string]CapabilityRecord `bun:"-" json:"capabilities"`
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
	ModelRowID   int64                       `bun:"model_row_id" json:"-"`
	ModelID      string                      `bun:"model_id" json:"model"`
	Tier         string                      `bun:"tier" json:"tier,omitempty"`
	Keyless      *bool                       `bun:"keyless" json:"keyless"`
	ChatCapable  bool                        `bun:"chat_capable" json:"chat_capable"`
	LastChecked  *time.Time                  `bun:"last_checked" json:"last_checked"`
	LastSeen     time.Time                   `bun:"last_seen" json:"last_seen"`
	Attempts     int                         `bun:"attempts" json:"recent_attempts"`
	Successes    int                         `bun:"successes" json:"recent_successes"`
	Capabilities map[string]CapabilityRecord `bun:"-" json:"capabilities"`
}

// Capability returns the record for one feature, and whether we hold one at
// all. Templates use the second return rather than a zero value so "never
// asked" cannot be printed as "no".
func (row ModelRow) Capability(name string) (CapabilityRecord, bool) {
	record, held := row.Capabilities[name]
	return record, held
}

// Capability is the same lookup for the API's working-model rows.
func (m WorkingModel) Capability(name string) (CapabilityRecord, bool) {
	record, held := m.Capabilities[name]
	return record, held
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
//
// features narrows it to models where every named capability was verified
// working. The match is deliberately on supported IS TRUE and nothing else: a
// capability nobody has asked about yet must not be handed to an agent that
// said it needs one, and neither must a claim the provider made about itself.
func WorkingModels(
	ctx context.Context,
	db *database.DB,
	limit int,
	features []string,
) ([]WorkingModel, error) {
	// Chat models only unless the caller asked for a feature that only a
	// drawing model has. Without this an image model -- proven keyless by its
	// own image probe -- would be handed to a caller as something to chat with.
	modality := "AND m.chat_capable"
	if slices.Contains(features, CapabilityImageOut) {
		modality = "AND NOT m.chat_capable"
	}
	var rows []WorkingModel
	err := db.Bun().NewRaw(`
		SELECT e.slug, e.base_url, e.chat_path, e.openai_compatible,
		       m.id AS model_row_id, m.model_id, m.tier, m.chat_capable, m.last_ok,
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
		WHERE m.keyless IS TRUE AND e.active `+modality+`
		  AND (SELECT count(*) FROM llm_model_capabilities c
		       WHERE c.model_id = m.id AND c.supported IS TRUE
		         AND c.capability IN (?)) = ?
		ORDER BY m.last_ok DESC NULLS LAST
		LIMIT ?
	`, bun.In(featureList(features)), len(features), limit).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ModelRowID)
	}
	byModel, err := capabilityRecords(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	for index := range rows {
		rows[index].Capabilities = byModel[rows[index].ModelRowID]
	}
	return rows, nil
}

// featureList keeps the IN clause syntactically valid when nothing was asked
// for. The surrounding count is compared against zero in that case, so the
// placeholder value can never match anything and never needs to.
func featureList(features []string) []string {
	if len(features) == 0 {
		return []string{""}
	}
	return features
}

// capabilityRecords reads the verdicts for rows that have already been fetched.
// It is a second query rather than an aggregate inside the first because the
// alternative is building JSON in SQL and taking it apart again in Go, and the
// row count here is a page of models, not a table scan.
//
// Every model gets an entry, empty where nothing has been asked yet, so a
// consumer reading the payload sees {} rather than null and a template asking
// for a capability gets a clean miss instead of a false "no".
func capabilityRecords(
	ctx context.Context,
	db *database.DB,
	ids []int64,
) (map[int64]map[string]CapabilityRecord, error) {
	byModel := map[int64]map[string]CapabilityRecord{}
	for _, id := range ids {
		byModel[id] = map[string]CapabilityRecord{}
	}
	if len(ids) == 0 {
		return byModel, nil
	}
	var records []CapabilityRecord
	if err := db.Bun().NewRaw(`
		SELECT model_id, capability, claimed, supported, last_checked, last_ok, last_error
		FROM llm_model_capabilities
		WHERE model_id IN (?)
	`, bun.In(ids)).Scan(ctx, &records); err != nil {
		return nil, err
	}
	for _, record := range records {
		byModel[record.ModelRowID][record.Capability] = record
	}
	return byModel, nil
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
		SELECT m.id AS model_row_id, m.model_id, m.tier, m.keyless, m.chat_capable,
		       m.last_checked, m.last_seen,
		       COALESCE(r.attempts, 0)  AS attempts,
		       COALESCE(r.successes, 0) AS successes
		FROM llm_models m
		JOIN llm_endpoints e ON e.id = m.endpoint_id
		LEFT JOIN (`+recentChatReliability+`) r
		       ON r.endpoint_id = m.endpoint_id AND r.model_used = m.model_id
		WHERE e.slug = ?
		ORDER BY m.keyless DESC NULLS LAST, m.model_id
	`, slug).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ModelRowID)
	}
	byModel, err := capabilityRecords(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	for index := range rows {
		rows[index].Capabilities = byModel[rows[index].ModelRowID]
	}
	return rows, nil
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
