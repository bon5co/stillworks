package audit

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
		AuthMode:         "none",
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
		Notes: "Claimed permanent free anonymous tier, roughly 2 requests per minute per IP per model, no signup. Rate limit is low enough that a single probe per cycle is the ceiling of what is polite.",
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
