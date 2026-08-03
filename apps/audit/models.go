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
	CreatedAt        time.Time `bun:"created_at,nullzero,notnull,default:now()"`
	UpdatedAt        time.Time `bun:"updated_at,nullzero,notnull,default:now()"`
}

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

// Probe kinds. One of each per endpoint per cycle, never more.
const (
	KindModels = "models"
	KindChat   = "chat"
)

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
