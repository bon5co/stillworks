package audit

import "net/url"

// SeedEndpoints are *claims*, not facts. The prober decides which of them is
// telling the truth today, and the answer is expected to change.
//
// There are two shelves here and they are not the same product.
//
// AuthMode none is the flagship: somewhere that publicly says it answers LLM
// calls with no key and no signup. Rules for adding one:
//   - It must be reachable with zero credentials. A free *tier* that needs a
//     signup is not this, however free it is.
//   - Note where the claim came from, so a wrong entry can be traced.
//   - Never seed a URL nobody has published. A guessed endpoint is noise.
//
// AuthMode key is the second shelf, added 2026-08-04 after a full sweep of
// every plausible keyless endpoint on the internet turned up no new ones: the
// genuinely keyless universe is about four providers, and it is not growing.
// The thing worth publishing was never the word keyless, it was that we
// re-probe and say what happened. Rules for adding one:
//   - We must hold a free-tier key for it, obtained without a card and without
//     a phone number, and KeyEnv must name the variable it lives in.
//   - The endpoint must be probeable politely inside a free tier's own limits.
//   - Every published result says it was measured on our key. A key-required
//     row never claims anything about what a new signup gets, because we have
//     no way to find that out.
var SeedEndpoints = []Endpoint{
	{
		Slug:             "llm7",
		Provider:         "LLM7.io",
		BaseURL:          "https://api.llm7.io",
		ChatPath:         "/v1/chat/completions",
		ModelsPath:       "/v1/models",
		AuthMode:         AuthModeNone,
		DefaultModel:     "",
		DocsURL:          "https://llm7.io",
		OpenAICompatible: true,
		Active:           true,
		Notes: "Verified keyless 2026-08-02 with no Authorization header: 35 models listed, " +
			"only the turbo tier answers (codestral-latest, gpt-oss:20b, " +
			"meta-Llama-3.1-8B-Instruct-Turbo, minimax-m2.7). The other 31 are pro and 401 " +
			"with missing_api_key. No rate-limit headers are returned, so quota is invisible " +
			"until it 429s.",
	},
	{
		Slug:             "pollinations",
		Provider:         "Pollinations",
		BaseURL:          "https://text.pollinations.ai",
		ChatPath:         "/openai",
		ModelsPath:       "/models",
		AuthMode:         AuthModeNone,
		DefaultModel:     "openai",
		DocsURL:          "https://github.com/pollinations/pollinations",
		OpenAICompatible: true,
		Active:           true,
		// The image surface is a different host with a different shape: a GET
		// on a prompt URL that answers with the picture itself, and its own
		// model listing that the text listing knows nothing about.
		ImagePath:       "https://image.pollinations.ai/prompt/",
		ImageMode:       ImageModePromptURL,
		ImageModelsPath: "https://image.pollinations.ai/models",
		Notes:           "Claimed keyless, OpenAI-compatible surface at /openai. Serves GPT-OSS class models with no account.",
	},
	{
		Slug:             "ovh-anonymous",
		Provider:         "OVHcloud AI Endpoints",
		BaseURL:          "https://oai.endpoints.kepler.ai.cloud.ovh.net",
		ChatPath:         "/v1/chat/completions",
		ModelsPath:       "/v1/models",
		AuthMode:         AuthModeNone,
		DefaultModel:     "",
		DocsURL:          "https://endpoints.ai.cloud.ovh.net/",
		OpenAICompatible: true,
		Active:           true,
		// Verified 2026-08-04: POST /v1/images/generations answers a keyless
		// request with a 3.3 MB OpenAI images envelope carrying a real PNG. The
		// image models are already in the same /v1/models listing as the chat
		// ones, so no second listing call is needed here.
		ImagePath: "/v1/images/generations",
		ImageMode: ImageModeOpenAI,
		// One probe per cycle, which is what the note below has said since the
		// endpoint was seeded and what the code did not do until the budget
		// became a column. Five of eight capability probes came back 429 on
		// 2026-08-04, which was us measuring their rate limiter.
		ChatProbes: 1,
		Notes:      "Claimed permanent free anonymous tier, roughly 2 requests per minute per IP per model, no signup. Rate limit is low enough that a single probe per cycle is the ceiling of what is polite.",
	},
	{
		Slug:             "mlvoca",
		Provider:         "mlvoca.com",
		BaseURL:          "https://mlvoca.com",
		ChatPath:         "/api/generate",
		ModelsPath:       "/api/tags",
		AuthMode:         AuthModeNone,
		DefaultModel:     "tinyllama",
		DocsURL:          "https://mlvoca.com/",
		OpenAICompatible: false,
		Active:           true,
		Notes:            "Ollama-shaped API, not OpenAI-compatible: /api/generate and /api/tags. Claimed no key and no published rate limit.",
	},

	// ---- The keyed shelf. Everything below needs a key and says so. ----
	//
	// All three accounts were opened on 2026-08-04 with one identity, no
	// payment card, and no phone number. Cerebras was attempted and is not
	// here: its onboarding now offers only a card or a $5 credit that is never
	// actually granted, and every chat call on the resulting key answers 402
	// payment_required. A key that cannot make a call is not a free tier, so
	// there is nothing to publish about it.

	{
		Slug:     "groq",
		Provider: "Groq",
		BaseURL:  "https://api.groq.com",
		// Groq mounts its OpenAI surface under /openai/v1, so the base URL a
		// client should be handed is https://api.groq.com/openai/v1 -- which is
		// what OpenAIBaseURL derives from these two fields.
		ChatPath:         "/openai/v1/chat/completions",
		ModelsPath:       "/openai/v1/models",
		AuthMode:         AuthModeKey,
		KeyEnv:           "GROQ_API_KEY",
		DefaultModel:     "llama-3.1-8b-instant",
		DocsURL:          "https://console.groq.com/docs/overview",
		OpenAICompatible: true,
		Active:           true,
		Notes: "Free tier, key required. Account opened 2026-08-04 with Google sign-in only: no card, no phone " +
			"number, no verification email, no onboarding form. Listing read with our key on 2026-08-04 " +
			"returned 15 models, including Whisper speech-to-text and Orpheus speech models that no chat " +
			"probe will ever touch. Published free-plan limits on that date: 30 requests per minute across " +
			"chat models, and per-day caps from 250 (groq/compound) through 1,000 (llama-3.3-70b-versatile, " +
			"the gpt-oss pair, qwen3.6-27b) to 14,400 (llama-3.1-8b-instant). Three probes an hour sits far " +
			"inside all of those.",
	},
	{
		Slug:             "mistral",
		Provider:         "Mistral AI",
		BaseURL:          "https://api.mistral.ai",
		ChatPath:         "/v1/chat/completions",
		ModelsPath:       "/v1/models",
		AuthMode:         AuthModeKey,
		KeyEnv:           "MISTRAL_API_KEY",
		DefaultModel:     "mistral-small-latest",
		DocsURL:          "https://docs.mistral.ai/",
		OpenAICompatible: true,
		Active:           true,
		Notes: "Free tier, key required. Account opened 2026-08-04 with Google sign-in: no card, and " +
			"contrary to what is widely repeated about Mistral, no phone number was demanded at any point. " +
			"Listing read with our key that day returned 52 models; the listing publishes a capabilities " +
			"block per model, so embeddings, OCR, moderation and Voxtral audio models are excluded from chat " +
			"probing by the provider's own completion_chat flag rather than by guessing from the name. The " +
			"free plan is a 10 US dollar monthly allowance shared across the whole product rather than a " +
			"request count, so our probes spend real credit -- a handful of 32-token calls an hour, which is " +
			"a rounding error against it, and the reason the budget is not raised.",
	},
	{
		Slug:     "openrouter",
		Provider: "OpenRouter",
		BaseURL:  "https://openrouter.ai",
		ChatPath: "/api/v1/chat/completions",
		// Deliberately the filtered listing, not the whole catalogue. The bare
		// /api/v1/models answered with 338 models on 2026-08-04, of which 16
		// cost nothing; the other 322 answer 404 "No endpoints found" to a
		// free-tier key, so probing them would spend a 50-request daily
		// allowance discovering, every day, that we still have no credit.
		// Recorded as an absolute URL because the query string is part of the
		// question being asked.
		ModelsPath:       "https://openrouter.ai/api/v1/models?max_price=0",
		AuthMode:         AuthModeKey,
		KeyEnv:           "OPENROUTER_API_KEY",
		DefaultModel:     "openai/gpt-oss-20b:free",
		DocsURL:          "https://openrouter.ai/docs",
		OpenAICompatible: true,
		Active:           true,
		// One per cycle. A zero-balance account is allowed 50 requests a day
		// against the free models; three an hour would be 72 before the daily
		// capability cycle asked for anything, so the shelf would spend most of
		// its time publishing our own rate limiting as though it were theirs.
		ChatProbes: 1,
		Notes: "Free tier, key required. Account opened 2026-08-04 with Google sign-in and a terms checkbox: " +
			"no card, no phone number. This row reads the zero-price slice of the catalogue: 16 of 338 models " +
			"cost nothing on that date, 14 of them carrying the :free suffix. Two facts measured here that " +
			"the marketing does not lead with -- meta-llama/llama-3.3-70b-instruct:free, which every free-LLM " +
			"list still names, returns 404 and exists only as a paid slug; and the free models are shared " +
			"upstream pools that answer 429 under load rather than queueing.",
	},
}

// BrowserCallOrigins is the exact set of origins the shelf's test-call button
// may reach from a visitor's browser, for the page's Content-Security-Policy.
// It is derived from the seed list rather than written out separately so the
// policy cannot drift from the shelf: an endpoint added here becomes callable,
// and nothing else ever does.
//
// Measured 2026-08-04: api.llm7.io, text.pollinations.ai and OVH's anonymous
// endpoint all answer a cross-origin preflight and the POST itself with
// Access-Control-Allow-Origin: *, so a browser call genuinely completes. An
// endpoint that does not is not a problem to solve here -- the page falls back
// to the server route and says so.
//
// An endpoint added straight to the database and not to this list will not be
// in the policy, so its button falls back to the server route until a deploy.
// That is the safe direction to fail in.
//
// Keyed endpoints are excluded, and not as an oversight. There is no test-call
// button on the keyed shelf -- the call could only be made on our credential --
// so widening the policy to those hosts would grant the page a reach it has no
// use for. A Content-Security-Policy that lists origins nothing calls is a
// policy that has stopped describing the site.
func BrowserCallOrigins() []string {
	seen := make(map[string]struct{}, len(SeedEndpoints))
	origins := make([]string, 0, len(SeedEndpoints))
	for _, endpoint := range SeedEndpoints {
		if endpoint.RequiresKey() {
			continue
		}
		parsed, err := url.Parse(endpoint.BaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		origin := parsed.Scheme + "://" + parsed.Host
		if _, already := seen[origin]; already {
			continue
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	return origins
}
