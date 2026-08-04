package audit

import (
	"time"

	"github.com/uptrace/bun"
)

// Endpoint is a *claim* that a base URL answers LLM calls with no key and no
// signup. Nothing here is trusted; only a Probe row decides whether it is true.
type Endpoint struct {
	bun.BaseModel `bun:"table:llm_endpoints,alias:e"`

	ID               int64     `bun:"id,pk,autoincrement"`
	Slug             string    `bun:"slug,notnull"`
	Provider         string    `bun:"provider,notnull"`
	BaseURL          string    `bun:"base_url,notnull"`
	ChatPath         string    `bun:"chat_path,notnull"`
	ModelsPath       string    `bun:"models_path,notnull"`
	AuthMode         string    `bun:"auth_mode,notnull"`
	DefaultModel     string    `bun:"default_model,notnull"`
	DocsURL          string    `bun:"docs_url,notnull"`
	Notes            string    `bun:"notes,notnull"`
	OpenAICompatible bool      `bun:"openai_compatible,notnull"`
	Active           bool      `bun:"active,notnull"`
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

	ID          int64      `bun:"id,pk,autoincrement"`
	EndpointID  int64      `bun:"endpoint_id,notnull"`
	ModelID     string     `bun:"model_id,notnull"`
	Tier        string     `bun:"tier,notnull"`
	ChatCapable bool       `bun:"chat_capable,notnull"`
	InputModes  string     `bun:"input_modalities,notnull"`
	OutputModes string     `bun:"output_modalities,notnull"`
	Keyless     *bool      `bun:"keyless"`
	FirstSeen   time.Time  `bun:"first_seen,nullzero,notnull,default:now()"`
	LastSeen    time.Time  `bun:"last_seen,nullzero,notnull,default:now()"`
	LastChecked *time.Time `bun:"last_checked"`
	LastOK      *time.Time `bun:"last_ok"`
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
