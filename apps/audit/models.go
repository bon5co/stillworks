package audit

import (
	"time"

	"github.com/uptrace/bun"
)

// Auth modes. AuthModeNone is the flagship shelf's whole claim: no key, no
// signup, nothing to sign up for. AuthModeKey is the second shelf: a provider
// whose free tier we hold a key for, probed with that key.
//
// The distinction is never presented as a shade of the same thing. A keyless
// verdict is a fact about what anybody can call; a keyed verdict is a fact
// about what *our* account got today, and a new signup's quota is not something
// this site can measure for anyone.
const (
	AuthModeNone = "none"
	AuthModeKey  = "key"
	// AuthModeAny is a query filter, never a stored value. It exists so a caller
	// can ask for both shelves in one request and get every row labelled, rather
	// than making two calls and merging them without the labels.
	AuthModeAny = "any"
)

// KnownAuthMode reports whether a name is one the API accepts as a filter.
func KnownAuthMode(mode string) bool {
	switch mode {
	case AuthModeNone, AuthModeKey, AuthModeAny:
		return true
	default:
		return false
	}
}

// AuthModes is the published set, for the error a bad ?auth= value earns. Same
// reasoning as Capabilities: a 400 that does not name the valid values just
// moves the guessing to the caller.
var AuthModes = []string{AuthModeNone, AuthModeKey, AuthModeAny}

// Endpoint is a *claim* that a base URL answers LLM calls -- with no key at all
// where AuthMode is none, or with the free-tier key named by KeyEnv where it is
// key. Nothing here is trusted; only a Probe row decides whether it is true.
type Endpoint struct {
	bun.BaseModel `bun:"table:llm_endpoints,alias:e"`

	ID         int64  `bun:"id,pk,autoincrement"`
	Slug       string `bun:"slug,notnull"`
	Provider   string `bun:"provider,notnull"`
	BaseURL    string `bun:"base_url,notnull"`
	ChatPath   string `bun:"chat_path,notnull"`
	ModelsPath string `bun:"models_path,notnull"`
	AuthMode   string `bun:"auth_mode,notnull"`
	// KeyEnv names the environment variable holding our free-tier key for this
	// provider. It is the name and never the value: this row is read by anything
	// with a psql prompt, and a secret that only ever lives in the process
	// environment cannot leak through a backup, a dump or a query.
	KeyEnv       string `bun:"key_env,notnull"`
	DefaultModel string `bun:"default_model,notnull"`
	// ChatProbes is this endpoint's share of a liveness cycle. It belongs to the
	// endpoint because the limit it respects does: OVH's anonymous tier allows
	// two requests a minute, OpenRouter's free tier fifty a day, and Groq's
	// fourteen thousand.
	//
	// Zero here is left to the column default rather than written, so a seed
	// row that does not care says nothing instead of asserting a number.
	ChatProbes       int    `bun:"chat_probes_per_cycle,nullzero,notnull,default:3"`
	DocsURL          string `bun:"docs_url,notnull"`
	Notes            string `bun:"notes,notnull"`
	OpenAICompatible bool   `bun:"openai_compatible,notnull"`
	Active           bool   `bun:"active,notnull"`
	// ImagePath and ImageMode describe the image generation surface, which is
	// never the chat path. ImageMode is "" when the provider offers none.
	ImagePath       string    `bun:"image_path,notnull"`
	ImageMode       string    `bun:"image_mode,notnull"`
	ImageModelsPath string    `bun:"image_models_path,notnull"`
	CreatedAt       time.Time `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt       time.Time `bun:"updated_at,nullzero,notnull,default:now()"`
}

// Image generation shapes actually served by keyless providers. Both were
// verified from this server on 2026-08-04.
const (
	// ImageModeOpenAI is POST {base}{image_path} with an OpenAI images request,
	// answered with a JSON envelope carrying b64_json. OVH serves this.
	ImageModeOpenAI = "openai"
	// ImageModePromptURL is GET {image_path}{url-escaped prompt}, answered with
	// the image bytes themselves. Pollinations serves this.
	ImageModePromptURL = "prompt_url"
)

// Model is one model id offered by an endpoint. Keyless is a property of the
// model, not the provider: on llm7.io only 4 of 35 models answer without a key.
// Keyless is a tri-state on purpose -- nil means never verified, which is not
// the same as verified-false.
type Model struct {
	bun.BaseModel `bun:"table:llm_models,alias:m"`

	ID          int64  `bun:"id,pk,autoincrement"`
	EndpointID  int64  `bun:"endpoint_id,notnull"`
	ModelID     string `bun:"model_id,notnull"`
	Tier        string `bun:"tier,notnull"`
	ChatCapable bool   `bun:"chat_capable,notnull"`
	InputModes  string `bun:"input_modalities,notnull"`
	OutputModes string `bun:"output_modalities,notnull"`
	Keyless     *bool  `bun:"keyless"`
	// AnsweredWithKey is the keyed shelf's verdict: this model answered a call
	// carrying our own free-tier key. It is a separate column from Keyless and
	// not a second meaning for it, so that no code path can turn a call we paid
	// for with a credential into a claim that anybody can make it for free.
	AnsweredWithKey *bool      `bun:"answered_with_key"`
	FirstSeen       time.Time  `bun:"first_seen,nullzero,notnull,default:now()"`
	LastSeen        time.Time  `bun:"last_seen,nullzero,notnull,default:now()"`
	LastChecked     *time.Time `bun:"last_checked"`
	LastOK          *time.Time `bun:"last_ok"`
}

// VerdictColumn is the column that records whether a model answered the kind of
// call this endpoint gets. It is one named function with a test rather than a
// conditional repeated at each write site, because it carries the rule the
// whole second shelf rests on: a probe that carried a key may never write
// keyless, and a probe that carried none may never write answered_with_key.
//
// Getting this wrong in either direction is invisible in the output -- both
// columns are booleans and both tables render "yes" -- and it would publish
// key-gated models on a shelf headed "no key, no signup".
func (e Endpoint) VerdictColumn() string { return verdictColumnFor(e.AuthMode) }

// verdictColumnFor is the same rule for the read side, where the auth mode
// arrives as a route's constant rather than as a whole Endpoint. It is one
// function so the two sides cannot drift: a shelf that filtered on one column
// and ordered by the other would be quietly wrong in a way no test of either
// half would catch.
func verdictColumnFor(auth string) string {
	if auth == AuthModeKey {
		return "answered_with_key"
	}
	return "keyless"
}

// RequiresKey reports whether probing this endpoint means sending a credential.
func (e Endpoint) RequiresKey() bool { return e.AuthMode == AuthModeKey }

// ChatProbesPerCycle is how many chat probes this endpoint gets, falling back
// to the shared default. A zero here means a row written before the column
// existed, or an Endpoint built by hand in a test; neither should mean "probe
// it zero times" and silently stop checking an endpoint forever.
func (e Endpoint) ChatProbesPerCycle() int {
	if e.ChatProbes <= 0 {
		return defaultChatProbesPerCycle
	}
	return e.ChatProbes
}

// Verified reports whether this model has answered the kind of call its
// endpoint gets. Used to decide which models are worth asking capability
// questions of: on a keyed endpoint the answer comes from answered_with_key,
// because keyless is -- correctly -- never written there at all.
func (m Model) Verified(endpoint Endpoint) bool {
	verdict := m.Keyless
	if endpoint.RequiresKey() {
		verdict = m.AnsweredWithKey
	}
	return verdict != nil && *verdict
}

// Probe outcomes. These deliberately separate "the service said no" from "the
// service was not there". Conflating them is exactly how every other free-LLM
// list goes stale: a provider quietly starts demanding a key, the list keeps
// showing a green tick because something answered on the socket.
const (
	OutcomeOK       = "ok"
	OutcomeNeedsKey = "needs_key"
	// OutcomeBlocked is a refusal that is not an authentication demand: an edge
	// or proxy turning us away, typically a bare 403 HTML page. Recording that
	// as needs_key would publish a false claim that a provider secretly wants a
	// key, which is the exact error this project exists to correct.
	OutcomeBlocked     = "blocked"
	OutcomeRateLimited = "rate_limited"
	OutcomeClientError = "client_error"
	OutcomeServerError = "server_error"
	OutcomeTimeout     = "timeout"
	OutcomeUnreachable = "unreachable"
	OutcomeBadBody     = "bad_body"
)

// Probe kinds. One of each per endpoint per cycle, never more. The capability
// kinds are the capability names themselves, so the raw probe log reads
// "tools | ok | 200" without a second column to decode.
const (
	KindModels      = "models"
	KindImageModels = "image_models"
	KindChat        = "chat"
)

// Capabilities are the features an agent actually has to know about before it
// can pick a model: an agent framework that needs tool calls cannot use a model
// that only chats, however fast and free it is.
const (
	CapabilityTools      = "tools"
	CapabilityJSONSchema = "json_schema"
	// CapabilityJSONObject is the weaker structured-output mode. It is tracked
	// separately rather than folded into json_schema because the two genuinely
	// come apart: llm7's meta-Llama-3.1-8B-Instruct-Turbo served a clean JSON
	// object for response_format json_object on 2026-08-04 and answered 405 to
	// the json_schema form of the same question.
	CapabilityJSONObject = "json_object"
	CapabilityVision     = "vision"
	CapabilityImageOut   = "image_out"
)

// Capabilities is the published set, in the order the shelf shows them. It is
// also the list handed back when /api/llm/up is asked to filter on a name that
// does not exist -- a 400 that does not say what the valid names are just moves
// the guessing to the caller.
var Capabilities = []string{
	CapabilityTools,
	CapabilityJSONSchema,
	CapabilityJSONObject,
	CapabilityVision,
	CapabilityImageOut,
}

// ChatCapabilities are the ones probed through the chat path.
var ChatCapabilities = []string{
	CapabilityTools,
	CapabilityJSONSchema,
	CapabilityJSONObject,
	CapabilityVision,
}

// KnownCapability reports whether a name is one we publish a verdict for.
func KnownCapability(name string) bool {
	for _, capability := range Capabilities {
		if capability == name {
			return true
		}
	}
	return false
}

// ModelCapability is one feature verdict for one model. supported is tri-state
// for the same reason keyless is: never verified is not a "no", and publishing
// it as one would be the same lie the rest of the internet's free-LLM lists
// tell about liveness.
type ModelCapability struct {
	bun.BaseModel `bun:"table:llm_model_capabilities,alias:c"`

	ID         int64  `bun:"id,pk,autoincrement"`
	ModelRowID int64  `bun:"model_id,notnull"`
	Capability string `bun:"capability,notnull"`
	// Claimed is what the provider's own listing says, where it says anything.
	// Kept so the shelf can show a claim next to the measurement instead of
	// quietly replacing one with the other.
	Claimed     *bool      `bun:"claimed"`
	Supported   *bool      `bun:"supported"`
	LastChecked *time.Time `bun:"last_checked"`
	LastOK      *time.Time `bun:"last_ok"`
	LastError   string     `bun:"last_error,notnull"`
}

// Probe is one recorded attempt. The table is append-only: an endpoint that
// dies between cycles must show up as history, because the track record is the
// product.
type Probe struct {
	bun.BaseModel `bun:"table:llm_probes,alias:p"`

	ID              int64     `bun:"id,pk,autoincrement"`
	EndpointID      int64     `bun:"endpoint_id,notnull"`
	StartedAt       time.Time `bun:"started_at,nullzero,notnull,default:now()"`
	Kind            string    `bun:"kind,notnull"`
	Outcome         string    `bun:"outcome,notnull"`
	HTTPStatus      int       `bun:"http_status,notnull"`
	LatencyMS       int       `bun:"latency_ms,notnull"`
	ModelUsed       string    `bun:"model_used,notnull"`
	ModelsListed    int       `bun:"models_listed,notnull"`
	CompletionChars int       `bun:"completion_chars,notnull"`
	Error           string    `bun:"error,notnull"`
}

// Working reports whether this probe counts as the endpoint actually working.
func (p Probe) Working() bool { return p.Outcome == OutcomeOK }
