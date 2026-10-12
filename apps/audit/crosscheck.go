package audit

import (
	"fmt"
	"strings"
)

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
// Of those 226, eighteen were typed as needing no key or an optional one. Those
// eighteen are what the first sweep checked; the second (CrosscheckSweeps) went
// back on 2026-10-12 to a registry grown to 270 providers.
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
// Verified 2026-08-11 and 2026-10-12 UTC from this service's own address. Per-IP quotas make
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

// CrosscheckSweep is one pass over the source catalogue. The numbers are
// counted from the catalogue's own files on the date given, not remembered, so
// a later sweep can be compared with an earlier one on the same footing.
type CrosscheckSweep struct {
	Date string
	// Providers is how many provider entries the registry held that day.
	Providers int
	// Claims is how many of them its own authType marks as needing no key or
	// an optional one. Kept separate from what we then probed, because the
	// second sweep also called providers the catalogue labels free in other
	// ways (a public placeholder key, a "keyless" tag in its free-tier list).
	Claims int
	// Outcome is the one-line result printed under the shelf.
	Outcome string
}

// CrosscheckSweeps is newest last. The page prints the last one and names the
// first, which is the comparison a reader needs: how big the catalogue is now,
// and whether the earlier answer survived it.
var CrosscheckSweeps = []CrosscheckSweep{
	{
		Date: "2026-08-11", Providers: 226, Claims: 18,
		Outcome: "One new claim on the shelf and no new working endpoint.",
	},
	{
		Date: "2026-10-12", Providers: 270, Claims: 17,
		Outcome: "Two new working keyless endpoints, Kilo Gateway and Free.ai, now on the shelf. " +
			"Every other claim failed the chat call, and is listed below with the answer it gave.",
	},
}

// crosscheckCaption is the sentence under the fold. It is built from the sweeps
// so the page cannot say 226 after a sweep that found 270.
func crosscheckCaption() string {
	var parts []string
	for _, sweep := range CrosscheckSweeps {
		parts = append(parts, fmt.Sprintf(
			"%s: %d providers, %d typed as needing no key. %s",
			sweep.Date, sweep.Providers, sweep.Claims, sweep.Outcome))
	}
	return "Cross-checked against OmniRoute's routing registry. " + strings.Join(parts, " ")
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
		Verdict:  "401 invalid_key on both the listing and the chat call.",
	},
	{
		Name:     "TheOldLLM",
		Endpoint: "https://theoldllm.vercel.app/api/chatgpt",
		Verdict: "403 at the edge. The same catalogue's own free-tier research lists this provider as having " +
			"no current free tier, so the registry entry and the documentation disagree; the call settles it.",
	},
	{
		Name:     "AI Horde",
		Endpoint: "https://oai.aihorde.net/v1/chat/completions",
		Verdict: "401 {\"detail\":\"Authorization header missing\"} with no header, on 2026-10-12. The same call " +
			"with its documented public anonymous key (ten zeros) as a bearer token answered 200 on " +
			"koboldcpp/Angelic_Eclipse-12B, so it is free, and open to anyone, but it is a key: this shelf " +
			"lists only calls that carry no Authorization header at all. Volunteer-worker queue, latency in " +
			"minutes on a busy day.",
	},
	{
		Name:     "OpenCode Free",
		Endpoint: "https://opencode.ai/zen/v1/chat/completions",
		Verdict: "Listing answers a bare GET with 200 and names the free models; the chat call answers 403 " +
			"FreeTierError: \"OpenCode's free tier can only be used from within OpenCode\". The body says " +
			"why, so this is a policy and not a bare 403 -- the free tier is gated on the client, not the " +
			"credential, and an OpenAI client is not that client.",
	},
	{
		Name:     "Cloudflare AI Playground",
		Endpoint: "https://playground.ai.cloudflare.com",
		Verdict: "405 on POST /chat/completions. The source catalogue reaches it by driving a headless " +
			"Chromium against the playground page, not by an API: there is no OpenAI-style endpoint to hand a " +
			"client. Same class as the scraped web UIs below, and rejected for the same reason.",
	},
	{
		Name:     "UC (uncensored.com persona)",
		Endpoint: "wss://internal-6.pubyar.com",
		Verdict: "Not an HTTP chat endpoint. The catalogue's own source describes a WebSocket session minted " +
			"from an account cookie after an email-code login on a subscription product. That is an account, " +
			"not a free call, and it cannot be probed by a request with no credential. (GET / on the host " +
			"answers 200 with a status object; POST /chat/completions answers 404.)",
	},
	{
		Name:     "FriendliAI",
		Endpoint: "https://api.friendli.ai/serverless/v1",
		Verdict: "The catalogue's free-tier list tags it keyless. Listing answers 200 and the chat call " +
			"answers 401 {\"detail\":\"Unauthorized. Please provide a valid credential.\"}: the listing-is-" +
			"not-an-endpoint shape again.",
	},
	{
		Name:     "Liquid AI",
		Endpoint: "https://inference.liquid.ai/v1",
		Verdict: "Tagged keyless in the catalogue's free-tier list. Both the listing and the chat call answer " +
			"403 {\"detail\":\"Not authenticated\"}. The body says who is missing, so this is a request " +
			"for a credential and not a block.",
	},
	{
		Name:     "Blackbox AI",
		Endpoint: "https://api.blackbox.ai/v1",
		Verdict: "Tagged keyless in the catalogue's free-tier list. 404 on both the listing and the chat call at " +
			"the URL the catalogue gives, and 404 on /chat/completions at the host root, so nothing here " +
			"answered a chat call with or without a key.",
	},
	{
		Name:     "iFlytek Spark / SparkDesk",
		Endpoint: "https://spark-api-open.xf-yun.com/v1",
		Verdict: "Tagged keyless in the catalogue's free-tier list. 401 {\"message\":\"Unauthorized\"} on the " +
			"chat call. The two catalogue entries are the same URL.",
	},
	{
		Name:     "Registry entries added since 2026-08-11",
		Endpoint: "52 providers, one anonymous chat call each, 2026-10-12",
		Verdict: "None of these is typed keyless by the catalogue, so none was a claim; they were called anyway " +
			"because a typed key and a working keyless call are not the same thing, and Free.ai is the " +
			"proof. Of 52 calls: 38 answered 401 or a body asking for a key, 3 answered 400 (one of which, " +
			"Free.ai, answered 200 once given a model it serves and is on the shelf), 6 answered 404 and " +
			"2 answered 405 because they are not OpenAI-shaped (browser-driven or proprietary paths -- not a " +
			"measurement of the provider), and 3 gave a 5xx from the host (void-ai, tabitoken, chat-oripe) " +
			"that says nothing about auth. Unmeasured is not rejected: those five-hundreds can be re-run.",
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
		Endpoint: "auggie, devin-cli-agentic, kiro, amazon-q, zcode, codex-app-server",
		Verdict: "Credential-free only in the sense that an installed CLI already holds an OAuth session. There " +
			"is no URL to hand an OpenAI client, so there is nothing here for this shelf to check.",
	},
}
