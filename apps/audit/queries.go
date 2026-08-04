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
	Slug     string `bun:"slug" json:"slug"`
	Provider string `bun:"provider" json:"provider"`
	BaseURL  string `bun:"base_url" json:"base_url"`
	ChatPath string `bun:"chat_path" json:"chat_path"`
	// AuthMode says which shelf this endpoint is on, and therefore what its
	// numbers mean. Every payload carrying a row has to state it: "verified"
	// against a keyless endpoint and "verified" against a keyed one are two
	// different claims that would otherwise print identically.
	AuthMode     string     `bun:"auth_mode" json:"auth_mode"`
	DocsURL      string     `bun:"docs_url" json:"docs_url"`
	Notes        string     `bun:"notes" json:"notes,omitempty"`
	Outcome      string     `bun:"outcome" json:"last_outcome"`
	CheckedAt    *time.Time `bun:"checked_at" json:"checked_at"`
	LatencyMS    int        `bun:"latency_ms" json:"latency_ms"`
	ModelsListed int        `bun:"models_listed" json:"models_listed"`
	// WorkingModels counts models that answered the kind of call this endpoint
	// gets, and KeyOnlyModels counts models that refused it. On a keyless
	// endpoint that refusal is a key demand; on a keyed one it is our own key
	// being turned away for a model outside the free tier.
	WorkingModels int `bun:"working_models" json:"working_models"`
	KeyOnlyModels int `bun:"key_only_models" json:"key_only_models"`
}

// RequiresKey reports whether this row belongs to the keyed shelf, so a view
// can label it without repeating the string comparison in every template.
func (row ShelfRow) RequiresKey() bool { return row.AuthMode == AuthModeKey }

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

// WorkingModel is a model verified to answer the kind of call its endpoint
// takes: with no key at all where AuthMode is none, and with our own free-tier
// key where it is key. The two are never merged into one boolean, because the
// second proves considerably less than the first.
type WorkingModel struct {
	Slug     string `bun:"slug" json:"endpoint"`
	BaseURL  string `bun:"base_url" json:"base_url"`
	ChatPath string `bun:"chat_path" json:"chat_path"`
	// AuthMode travels with every row, including on the keyless-only default,
	// so a caller that later opts into ?auth=any never has to guess which kind
	// of row it is holding and no row can be mistaken for the other kind.
	AuthMode         string     `bun:"auth_mode" json:"auth_mode"`
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

// RequiresKey reports whether calling this model means bringing a key.
func (m WorkingModel) RequiresKey() bool { return m.AuthMode == AuthModeKey }

// MarshalJSON adds the derived base URL so a consumer never has to work it out,
// and -- for a keyed row only -- the sentence that stops the row being read as
// the other kind. It rides on the row rather than only on the envelope because
// rows get copied out of payloads and pasted into other things, and a claim
// that loses its qualifier on the way is the failure this project is about.
func (m WorkingModel) MarshalJSON() ([]byte, error) {
	type payload WorkingModel
	return json.Marshal(struct {
		payload
		OpenAIBaseURL string `json:"openai_base_url,omitempty"`
		KeyNote       string `json:"key_note,omitempty"`
	}{
		payload:       payload(m),
		OpenAIBaseURL: openAIBaseIfCompatible(m),
		KeyNote:       keyNoteFor(m),
	})
}

// keyNote is the per-row qualifier on every key-required result. It says what
// was measured and whose credential measured it, and it declines to say the one
// thing a reader will want to assume: that a fresh signup gets the same answer.
// We cannot know that, so we do not imply it.
const keyNote = "Requires your own API key. Verified with stillworks' own free-tier key for this provider, " +
	"from this server's address. That proves the endpoint answers our account today — it is not a statement " +
	"about what a new signup's free tier includes, which this site cannot measure for you."

func keyNoteFor(m WorkingModel) string {
	if !m.RequiresKey() {
		return ""
	}
	return keyNote
}

func openAIBaseIfCompatible(m WorkingModel) string {
	if !m.OpenAICompatible {
		return ""
	}
	return m.OpenAIBaseURL()
}

// ModelRow is the per-endpoint model table.
type ModelRow struct {
	ModelRowID int64  `bun:"model_row_id" json:"-"`
	ModelID    string `bun:"model_id" json:"model"`
	Tier       string `bun:"tier" json:"tier,omitempty"`
	Keyless    *bool  `bun:"keyless" json:"keyless"`
	// AnsweredWithKey is the keyed shelf's verdict. Both columns are published
	// on every row rather than one merged field, so a reader can see which
	// question was actually asked of this model -- on a keyless endpoint
	// answered_with_key is always null, and that null is informative.
	AnsweredWithKey *bool                       `bun:"answered_with_key" json:"answered_with_key"`
	ChatCapable     bool                        `bun:"chat_capable" json:"chat_capable"`
	LastChecked     *time.Time                  `bun:"last_checked" json:"last_checked"`
	LastSeen        time.Time                   `bun:"last_seen" json:"last_seen"`
	Attempts        int                         `bun:"attempts" json:"recent_attempts"`
	Successes       int                         `bun:"successes" json:"recent_successes"`
	Capabilities    map[string]CapabilityRecord `bun:"-" json:"capabilities"`
}

// Verdict is the answer for whichever question this model's endpoint asks, so a
// template can render one column instead of choosing between two.
func (row ModelRow) Verdict(requiresKey bool) *bool {
	if requiresKey {
		return row.AnsweredWithKey
	}
	return row.Keyless
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

// verifiedModelCount is how many of an endpoint's models answered the kind of
// call that endpoint gets. It is one constant used by both the select list and
// the filter, so a row cannot be counted one way and filtered another.
const verifiedModelCount = `
	CASE WHEN e.auth_mode = 'key'
	     THEN COALESCE(w.answered_with_key, 0)
	     ELSE COALESCE(w.keyless, 0) END
`

const latestModelsProbe = `
	SELECT DISTINCT ON (endpoint_id) endpoint_id, outcome, started_at, latency_ms, models_listed
	FROM llm_probes
	WHERE kind = 'models'
	ORDER BY endpoint_id, started_at DESC
`

// authCondition narrows a query to one shelf. The auth mode is a property of
// the route being served, not of anything a visitor typed, so it is a separate
// argument from ShelfQuery rather than a field inside it: mixing the two would
// put "which shelf is this" within reach of a query string, and a URL that can
// quietly turn the keyless shelf into a mixed one is exactly the blurring this
// second shelf exists to avoid.
//
// The returned fragment is chosen from this fixed set and never built from
// input.
func authCondition(auth string) string {
	switch auth {
	case AuthModeKey:
		return "e.auth_mode = 'key'"
	case AuthModeAny:
		return "e.auth_mode IN ('none', 'key')"
	default:
		// Anything unrecognised falls back to the keyless shelf. A caller that
		// mistypes gets the narrower, stronger claim rather than the wider one.
		return "e.auth_mode = 'none'"
	}
}

// verifiedCondition is the "this answered us" test for one shelf. Each mode
// reads its own column, and the any-mode reads each endpoint's own: there is no
// expression here in which a keyed verdict could satisfy a keyless filter.
func verifiedCondition(auth string) string {
	switch auth {
	case AuthModeKey:
		return "m.answered_with_key IS TRUE"
	case AuthModeAny:
		return "((e.auth_mode = 'none' AND m.keyless IS TRUE)" +
			" OR (e.auth_mode = 'key' AND m.answered_with_key IS TRUE))"
	default:
		return "m.keyless IS TRUE"
	}
}

// ShelfMatching returns every active endpoint on one shelf with its most recent
// models probe and a count of models proven to answer, narrowed and ordered by
// an already-validated query. The ORDER BY arrives as a fragment chosen from a
// fixed table; every filter arrives as a bound parameter. Nothing a visitor
// typed is ever part of the SQL text.
func ShelfMatching(
	ctx context.Context,
	db *database.DB,
	query ShelfQuery,
	auth string,
) ([]ShelfRow, error) {
	conditions := []string{"e.active", authCondition(auth)}
	arguments := []any{}
	if query.Endpoint != "" {
		conditions = append(conditions, "e.slug = ?")
		arguments = append(arguments, query.Endpoint)
	}
	if query.Search != "" {
		// An endpoint matches the model search when it offers a matching model.
		// Matching the slug instead would answer a different question from the
		// one the same box answers on the table above it.
		conditions = append(conditions, `EXISTS (
			SELECT 1 FROM llm_models searched
			WHERE searched.endpoint_id = e.id AND searched.model_id ILIKE ? ESCAPE '\'
		)`)
		arguments = append(arguments, escapeLikePattern(query.Search))
	}
	switch query.Keyless {
	case keylessOnly:
		conditions = append(conditions, verifiedModelCount+" > 0")
	case keylessExclude:
		conditions = append(conditions, verifiedModelCount+" = 0")
	}

	// The counts are gathered for both shelves and chosen per row, because the
	// endpoint's own auth mode decides which column holds its verdict. A single
	// count over "keyless OR answered_with_key" would silently add the two
	// claims together, which is the one arithmetic this site must never do.
	var rows []ShelfRow
	err := db.Bun().NewRaw(`
		SELECT * FROM (
			SELECT e.slug, e.provider, e.base_url, e.chat_path, e.auth_mode, e.docs_url, e.notes,
			       COALESCE(p.outcome, 'never_probed') AS outcome,
			       p.started_at AS checked_at,
			       COALESCE(p.latency_ms, 0) AS latency_ms,
			       COALESCE(p.models_listed, 0) AS models_listed,
			       `+verifiedModelCount+` AS working_models,
			       CASE WHEN e.auth_mode = 'key'
			            THEN COALESCE(w.refused_our_key, 0)
			            ELSE COALESCE(w.key_only, 0) END AS key_only_models
			FROM llm_endpoints e
			LEFT JOIN (`+latestModelsProbe+`) p ON p.endpoint_id = e.id
			LEFT JOIN (
				SELECT endpoint_id,
				       count(*) FILTER (WHERE keyless IS TRUE)            AS keyless,
				       count(*) FILTER (WHERE keyless IS FALSE)           AS key_only,
				       count(*) FILTER (WHERE answered_with_key IS TRUE)  AS answered_with_key,
				       count(*) FILTER (WHERE answered_with_key IS FALSE) AS refused_our_key
				FROM llm_models GROUP BY endpoint_id
			) w ON w.endpoint_id = e.id
			WHERE `+strings.Join(conditions, " AND ")+`
		) endpoints
		ORDER BY `+query.endpointOrderBy()+`, slug
	`, arguments...).Scan(ctx, &rows)
	return rows, err
}

// WorkingModels lists every model verified to answer, freshest first. This is
// what the runtime API serves: an agent asking "what can I call right now".
//
// auth chooses the shelf and is the caller's explicit decision every time. It
// has no default here on purpose: /api/llm/up predates the second shelf and its
// callers have "no key needed" written into their code, so the one place that
// gets to assume keyless is the route that reads the query string, where the
// assumption is visible and tested.
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
	auth string,
) ([]WorkingModel, error) {
	return WorkingModelsMatching(ctx, db, limit, ParseShelfQuery(nil), features, auth)
}

// WorkingModelsMatching is WorkingModels narrowed and ordered by an
// already-validated query. The verified condition is not negotiable by that
// query: this list is what answered, and a visitor-supplied parameter that
// could widen it would make the same table mean two different things.
func WorkingModelsMatching(
	ctx context.Context,
	db *database.DB,
	limit int,
	query ShelfQuery,
	features []string,
	auth string,
) ([]WorkingModel, error) {
	conditions := []string{verifiedCondition(auth), authCondition(auth), "e.active"}
	arguments := []any{}
	// Chat models only, unless the caller asked for a feature that only a
	// drawing model has. Without this an image model -- proven keyless by its
	// own image probe -- would be handed to a caller as something to chat with.
	if slices.Contains(features, CapabilityImageOut) {
		conditions = append(conditions, "NOT m.chat_capable")
	} else {
		conditions = append(conditions, "m.chat_capable")
	}
	if query.Endpoint != "" {
		conditions = append(conditions, "e.slug = ?")
		arguments = append(arguments, query.Endpoint)
	}
	if query.Search != "" {
		conditions = append(conditions, `m.model_id ILIKE ? ESCAPE '\'`)
		arguments = append(arguments, escapeLikePattern(query.Search))
	}
	// Every named feature has to be verified working, so the count of matching
	// verdicts has to equal the count asked for. With nothing asked for the
	// comparison is against zero and the placeholder can never match anything.
	conditions = append(conditions, `(
		SELECT count(*) FROM llm_model_capabilities c
		WHERE c.model_id = m.id AND c.supported IS TRUE AND c.capability IN (?)
	) = ?`)
	arguments = append(arguments, bun.In(featureList(features)), len(features))
	arguments = append(arguments, limit)

	// The select is wrapped so the ORDER BY can work on the computed columns:
	// latency has to become NULLIF(latency_ms, 0) to sort unmeasured rows last,
	// and an output alias cannot be used inside an expression otherwise.
	var rows []WorkingModel
	err := db.Bun().NewRaw(`
		SELECT * FROM (
			SELECT e.slug, e.base_url, e.chat_path, e.openai_compatible, e.auth_mode,
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
			WHERE `+strings.Join(conditions, " AND ")+`
		) working
		ORDER BY `+query.workingOrderBy()+`, slug, model_id
		LIMIT ?
	`, arguments...).Scan(ctx, &rows)
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
// It looks across both shelves: an endpoint's own page is where its auth mode
// is spelled out, so hiding keyed endpoints from it would only mean a dead link
// from the shelf that lists them.
func EndpointBySlug(ctx context.Context, db *database.DB, slug string) (ShelfRow, bool, error) {
	rows, err := ShelfMatching(ctx, db, ParseShelfQuery(nil), AuthModeAny)
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

// EndpointSlugs is every active endpoint on one shelf, for that shelf's filter
// control. It ignores the current filter on purpose: a dropdown that only
// offers the value already selected is a dead end. It does not ignore the
// shelf: offering a keyed endpoint in the keyless shelf's dropdown would
// produce an empty table and read as an outage.
func EndpointSlugs(ctx context.Context, db *database.DB, auth string) ([]string, error) {
	var slugs []string
	err := db.Bun().NewRaw(`
		SELECT e.slug FROM llm_endpoints e WHERE e.active AND `+authCondition(auth)+`
		ORDER BY e.slug
	`).Scan(ctx, &slugs)
	return slugs, err
}

// KeyEnvsFor lists the environment variable names the active endpoints on one
// shelf read their credentials from. Only the names: the values never leave the
// process environment. It exists so a page can tell "nobody answered" apart
// from "this deployment holds no keys", which are the same empty table and very
// different facts -- one is about the providers and the other is about us, and
// publishing the second as the first is exactly the error this site is for.
func KeyEnvsFor(ctx context.Context, db *database.DB, auth string) ([]string, error) {
	var names []string
	err := db.Bun().NewRaw(`
		SELECT e.key_env FROM llm_endpoints e
		WHERE e.active AND e.key_env <> '' AND `+authCondition(auth)+`
	`).Scan(ctx, &names)
	return names, err
}

// TrackedModel returns the endpoint and the recorded keyless state for a model
// we already track, and reports false for anything else. It is what stops the
// test-call route from being a relay: a visitor can only ask us to call a pair
// the prober already put in the database.
func TrackedModel(
	ctx context.Context,
	db *database.DB,
	slug string,
	modelID string,
) (Endpoint, *bool, bool, error) {
	var found []struct {
		Endpoint
		Keyless *bool `bun:"keyless"`
	}
	err := db.Bun().NewRaw(`
		SELECT e.*, m.keyless
		FROM llm_endpoints e
		JOIN llm_models m ON m.endpoint_id = e.id
		WHERE e.active AND e.slug = ? AND m.model_id = ?
		LIMIT 1
	`, slug, modelID).Scan(ctx, &found)
	if err != nil || len(found) == 0 {
		return Endpoint{}, nil, false, err
	}
	return found[0].Endpoint, found[0].Keyless, true, nil
}

// ModelsFor lists what an endpoint advertises and what we have verified.
//
// auth decides which verdict column orders the table, and is taken from the
// endpoint rather than inferred from the data. Coalescing the two columns would
// read the wrong one for an endpoint that has been moved between shelves --
// seedllm updates auth_mode in place, so a row can carry a stale verdict from
// the shelf it used to be on, and sorting by it would put a model the current
// shelf has never checked above one it has.
func ModelsFor(ctx context.Context, db *database.DB, slug string, auth string) ([]ModelRow, error) {
	var rows []ModelRow
	err := db.Bun().NewRaw(`
		SELECT m.id AS model_row_id, m.model_id, m.tier, m.keyless, m.answered_with_key,
		       m.chat_capable, m.last_checked, m.last_seen,
		       COALESCE(r.attempts, 0)  AS attempts,
		       COALESCE(r.successes, 0) AS successes
		FROM llm_models m
		JOIN llm_endpoints e ON e.id = m.endpoint_id
		LEFT JOIN (`+recentChatReliability+`) r
		       ON r.endpoint_id = m.endpoint_id AND r.model_used = m.model_id
		WHERE e.slug = ?
		ORDER BY m.`+verdictColumnFor(auth)+` DESC NULLS LAST, m.model_id
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
