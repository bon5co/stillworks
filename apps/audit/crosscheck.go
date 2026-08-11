package audit

// Endpoints that a published catalogue calls free, that we checked, and that
// are not on the shelf.
//
// The shelf can only ever say what answered. It cannot say what was considered
// and thrown out, and that omission is the one a directory always makes: a list
// of ten working endpoints looks identical whether ten were checked or ninety
// were. So the rejects are published too, each with the call that rejected it.
//
// The source here is OmniRoute (github.com/diegosouzapw/OmniRoute), which is
// worth cross-checking precisely because it is not a link farm: 226 providers
// in a real routing registry, each carrying an authType, so its claim about
// which endpoints need no credential is machine-checkable rather than prose.
// Of those 226, eighteen are typed as needing no key or an optional one. Those
// eighteen are what this list is the result of checking.
//
// Rules for what is written here, mirroring the ones seed.go applies to a
// claim that gets in:
//   - The verdict names the call and the status code, so it can be re-run.
//   - A rejection is dated, because it is a measurement and will go stale.
//     Every one of these can become a shelf row later; none of them is a
//     judgement about the provider.
//   - Rejected for a reason that is not a status code -- a licence, a terms of
//     service clause, a vendor CLI's OAuth -- says which reason, because that
//     kind of rejection can never be re-derived from a probe.
//
// Verified 2026-08-11 UTC from this service's own address. Per-IP quotas make
// that vantage part of the result, exactly as they do on the shelf itself.
type RejectedClaim struct {
	// Name is the provider as the source catalogue names it.
	Name string
	// Endpoint is the URL that was called, not the provider's home page.
	Endpoint string
	// Verdict is the measured outcome, in the fewest words that still let
	// somebody re-run the call and get the same thing.
	Verdict string
}

// RejectedClaims is rendered under the shelf. It is deliberately a fixed list
// and not a probe result: an endpoint that never gets a row should not also get
// an hourly request from us forever. One check, published, is the whole of what
// we owe a claim that did not hold.
var RejectedClaims = []RejectedClaim{
	{
		Name:     "Hack Club AI",
		Endpoint: "https://ai.hackclub.com/proxy/v1",
		Verdict: "Listing answers a bare GET with 200 and a full model catalogue; the chat call answers " +
			"401 Authentication required. A keyless /v1/models is not a keyless endpoint, and this is the " +
			"shape most often mistaken for one. Separately scoped to Hack Club members by its own terms.",
	},
	{
		Name:     "Naga",
		Endpoint: "https://api.naga.ac/v1",
		Verdict: "Same shape: listing 200, chat 401 authentication_required, with the body directing you to " +
			"sign up. A free tier behind a signup is the keyed shelf's subject, not this one, and we hold no key.",
	},
	{
		Name:     "Pollinations (gen host)",
		Endpoint: "https://gen.pollinations.ai/v1",
		Verdict: "401 UNAUTHORIZED with no header. Worth stating plainly because the catalogue routes here on " +
			"the grounds that the older text.pollinations.ai host was retired -- and that older host answered " +
			"our keyless chat call with 200 on the same day. The retired endpoint is the one still serving " +
			"anonymously, so that is the one on the shelf.",
	},
	{
		Name:     "g4f.space",
		Endpoint: "https://g4f.space/api/{pollinations,groq,gemini,nvidia,ollama}/v1",
		Verdict: "402 insufficient_credits: \"No cake credits. Bake proof-of-work cakes ... to earn anonymous " +
			"usage.\" Anonymous, but not free of charge -- the price is compute rather than a key. Nothing an " +
			"OpenAI client can pay, so it cannot be published as an endpoint that works right now.",
	},
	{
		Name:     "MiMo (Xiaomi)",
		Endpoint: "https://api.xiaomimimo.com/v1",
		Verdict: "401 invalid_key on both the listing and the chat call.",
	},
	{
		Name:     "TheOldLLM",
		Endpoint: "https://theoldllm.vercel.app/api/chatgpt",
		Verdict: "403 at the edge. The same catalogue's own free-tier research lists this provider as having " +
			"no current free tier, so the registry entry and the documentation disagree; the call settles it.",
	},
	{
		Name:     "Web-UI scrapers",
		Endpoint: "duckduckgo-web, felo-web, qwen-web, t3-web, muse-spark-web, veoaifree-web, agy",
		Verdict: "Not probed, and not a measurement question. These drive somebody's chat website through an " +
			"adapter; the source catalogue's own terms-of-service table marks every one of them avoid for " +
			"proxied or non-personal use. An endpoint we would have to breach terms to call is not an endpoint " +
			"we can tell you to call.",
	},
	{
		Name:     "Vendor agent CLIs",
		Endpoint: "auggie, devin-cli-agentic, kiro, amazon-q, opencode",
		Verdict: "Credential-free only in the sense that an installed CLI already holds an OAuth session. There " +
			"is no URL to hand an OpenAI client, so there is nothing here for this shelf to check.",
	},
}
