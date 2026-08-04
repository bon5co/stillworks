package audit

import "net/url"

// SeedEndpoints are *claims*, not facts. Each one is somewhere that publicly
// says it answers LLM calls with no key and no signup. The prober decides which
// of them is telling the truth today, and the answer is expected to change.
//
// Rules for adding one here:
//   - It must be reachable with zero credentials. Free *tiers* that need a
//     signup belong on somebody else's list, not this shelf.
//   - Note where the claim came from, so a wrong entry can be traced.
//   - Never seed a URL nobody has published. A guessed endpoint is noise.
var SeedEndpoints = []Endpoint{
	{
		Slug:             "llm7",
		Provider:         "LLM7.io",
		BaseURL:          "https://api.llm7.io",
		ChatPath:         "/v1/chat/completions",
		ModelsPath:       "/v1/models",
		AuthMode:         "none",
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
		AuthMode:         "none",
		DefaultModel:     "openai",
		DocsURL:          "https://github.com/pollinations/pollinations",
		OpenAICompatible: true,
		Active:           true,
		Notes:            "Claimed keyless, OpenAI-compatible surface at /openai. Serves GPT-OSS class models with no account.",
	},
	{
		Slug:             "ovh-anonymous",
		Provider:         "OVHcloud AI Endpoints",
		BaseURL:          "https://oai.endpoints.kepler.ai.cloud.ovh.net",
		ChatPath:         "/v1/chat/completions",
		ModelsPath:       "/v1/models",
		AuthMode:         "none",
		DefaultModel:     "",
		DocsURL:          "https://endpoints.ai.cloud.ovh.net/",
		OpenAICompatible: true,
		Active:           true,
		Notes:            "Claimed permanent free anonymous tier, roughly 2 requests per minute per IP per model, no signup. Rate limit is low enough that a single probe per cycle is the ceiling of what is polite.",
	},
	{
		Slug:             "mlvoca",
		Provider:         "mlvoca.com",
		BaseURL:          "https://mlvoca.com",
		ChatPath:         "/api/generate",
		ModelsPath:       "/api/tags",
		AuthMode:         "none",
		DefaultModel:     "tinyllama",
		DocsURL:          "https://mlvoca.com/",
		OpenAICompatible: false,
		Active:           true,
		Notes:            "Ollama-shaped API, not OpenAI-compatible: /api/generate and /api/tags. Claimed no key and no published rate limit.",
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
func BrowserCallOrigins() []string {
	seen := make(map[string]struct{}, len(SeedEndpoints))
	origins := make([]string, 0, len(SeedEndpoints))
	for _, endpoint := range SeedEndpoints {
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
