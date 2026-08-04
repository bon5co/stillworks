package audit

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// The per-visitor limit is what stops one person spending a provider's free
// quota through this site. It has to refuse at the limit, not after it, and it
// has to say when it will stop refusing.
func TestVisitorRateLimitRefusesAtTheCeilingAndRecovers(t *testing.T) {
	clock := time.Now()
	limiter := newRateLimiter(3, time.Minute)
	limiter.now = func() time.Time { return clock }

	for attempt := 1; attempt <= 3; attempt++ {
		if allowed, _ := limiter.allow("visitor"); !allowed {
			t.Fatalf("attempt %d was refused inside the limit", attempt)
		}
	}
	allowed, retryIn := limiter.allow("visitor")
	if allowed {
		t.Fatal("the fourth attempt inside the window was allowed")
	}
	if retryIn <= 0 || retryIn > time.Minute {
		t.Fatalf("retry-in = %v, want something inside the window", retryIn)
	}

	// A different visitor is unaffected: the limit is per person, not a global
	// gate wearing a per-person name.
	if allowed, _ := limiter.allow("somebody else"); !allowed {
		t.Fatal("one visitor's limit refused another visitor")
	}

	clock = clock.Add(time.Minute + time.Second)
	if allowed, _ := limiter.allow("visitor"); !allowed {
		t.Fatal("the window did not clear")
	}
}

// The map cannot be allowed to grow with every address that ever arrives.
func TestRateLimiterForgetsKeysThatFellOutOfTheWindow(t *testing.T) {
	clock := time.Now()
	limiter := newRateLimiter(1, time.Minute)
	limiter.now = func() time.Time { return clock }

	for index := 0; index < limiterKeyCeiling+50; index++ {
		limiter.allow(string(rune(index)) + "-visitor")
		clock = clock.Add(time.Second)
	}
	if len(limiter.seen) > limiterKeyCeiling {
		t.Fatalf("limiter holds %d keys, above the ceiling of %d", len(limiter.seen), limiterKeyCeiling)
	}
}

func TestHumanDelayRoundsUpSoTheAdviceWorks(t *testing.T) {
	for delay, want := range map[time.Duration]string{
		20 * time.Second:               "under a minute",
		90 * time.Second:               "2 minutes",
		2*time.Minute + 40*time.Second: "3 minutes",
		9*time.Minute + 59*time.Second: "10 minutes",
	} {
		if got := humanDelay(delay); got != want {
			t.Errorf("humanDelay(%v) = %q, want %q", delay, got, want)
		}
	}
}

// The body sent for a test call has to match the shape the endpoint speaks, and
// it must never carry a key or a token budget larger than a demonstration.
func TestTryBodyMatchesTheEndpointShapeAndStaysSmall(t *testing.T) {
	openAI, err := tryBody(Endpoint{OpenAICompatible: true}, "gpt-oss:20b", "hello")
	if err != nil {
		t.Fatalf("openai body: %v", err)
	}
	for _, want := range []string{`"messages"`, `"max_tokens":96`, `"stream":false`} {
		if !strings.Contains(string(openAI), want) {
			t.Errorf("openai body missing %s: %s", want, openAI)
		}
	}
	ollama, err := tryBody(Endpoint{OpenAICompatible: false}, "tinyllama", "hello")
	if err != nil {
		t.Fatalf("ollama body: %v", err)
	}
	if !strings.Contains(string(ollama), `"prompt"`) || strings.Contains(string(ollama), `"messages"`) {
		t.Errorf("ollama body has the wrong shape: %s", ollama)
	}
	for _, body := range [][]byte{openAI, ollama} {
		if strings.Contains(strings.ToLower(string(body)), "authorization") ||
			strings.Contains(strings.ToLower(string(body)), "api_key") {
			t.Errorf("a test call carried credentials: %s", body)
		}
	}
}

// A result that does not say whose address made the call is the exact claim
// this site exists to stop other directories making.
func TestServerSideResultAlwaysNamesTheAddressItCameFrom(t *testing.T) {
	result := TryResult{From: FromServer}
	if !strings.Contains(result.From, "server") {
		t.Fatalf("From = %q, which does not say the call came from this server", result.From)
	}
	if !(TryResult{}).Made() {
		t.Fatal("a result with no refusal should count as a call that happened")
	}
	if (TryResult{Refused: "rate limited"}).Made() {
		t.Fatal("a refusal must not count as a call that happened")
	}
}

// The origins in the policy are the endpoints on the shelf, in a form a CSP can
// use: a scheme and a host, no path.
func TestBrowserCallOriginsAreBareOrigins(t *testing.T) {
	origins := BrowserCallOrigins()
	if len(origins) == 0 {
		t.Fatal("no origins derived from the seed list")
	}
	seen := map[string]struct{}{}
	for _, origin := range origins {
		parsed, err := url.Parse(origin)
		if err != nil {
			t.Fatalf("origin %q does not parse: %v", origin, err)
		}
		if parsed.Path != "" || parsed.RawQuery != "" {
			t.Errorf("origin %q carries more than a scheme and host", origin)
		}
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			t.Errorf("origin %q is not an HTTP origin", origin)
		}
		if _, already := seen[origin]; already {
			t.Errorf("origin %q listed twice", origin)
		}
		seen[origin] = struct{}{}
	}
	// Every seeded keyless endpoint has to be reachable from the page, or its
	// button silently falls back to the server route. Keyed endpoints have no
	// button -- the call would be made on our credential -- so they are absent
	// from the policy on purpose, and TestBrowserCallOriginsExcludeKeyedProviders
	// asserts that side of it.
	for _, endpoint := range SeedEndpoints {
		if endpoint.RequiresKey() {
			continue
		}
		parsed, _ := url.Parse(endpoint.BaseURL)
		if _, present := seen[parsed.Scheme+"://"+parsed.Host]; !present {
			t.Errorf("seeded keyless endpoint %s is missing from the policy", endpoint.Slug)
		}
	}
}

// A prompt is pasted into a single-quoted shell argument inside a JSON string.
// Both layers have to survive a visitor who types a quote.
func TestSnippetSurvivesAHostilePrompt(t *testing.T) {
	snippet := TrySnippet(TryResult{
		URL:    "https://api.llm7.io/v1/chat/completions",
		Model:  "gpt-oss:20b",
		Prompt: `it's "quoted" \ and multi` + "\n" + "line",
	})
	if strings.Contains(snippet, "\n  -H") == false {
		t.Fatalf("snippet lost its shape: %s", snippet)
	}
	// The prompt's own newline must not become a second shell line.
	promptLine := snippet[strings.Index(snippet, "-d '"):]
	if strings.Count(promptLine, "\n") != 0 {
		t.Errorf("a newline in the prompt broke the command: %s", snippet)
	}
	if strings.Contains(promptLine, `content":"it's`) {
		t.Errorf("an unescaped single quote would end the shell argument: %s", snippet)
	}
}

// An API caller has to be able to tell "ask again in a minute" from "that will
// never work" without reading the prose.
func TestRefusalStatusSeparatesRetryableFromPermanent(t *testing.T) {
	for reason, want := range map[string]int{
		RefusedUnknownPair: 400,
		RefusedCrawler:     403,
		RefusedRateLimit:   429,
		"":                 200,
	} {
		result := TryResult{RefusedBecause: reason}
		if got := result.RefusalStatus(); got != want {
			t.Errorf("refusal %q gave status %d, want %d", reason, got, want)
		}
	}
}
