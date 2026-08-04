package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bon5co/godjango/database"
)

// A test call exists because of the one thing this site cannot measure for
// anybody: keyless quotas are per-IP. An endpoint verified from this server can
// answer 402 or 429 from a visitor's address, and the honest way to settle that
// is to let them make the call themselves.
//
// The page tries the call from the visitor's own browser first. These limits
// govern the fallback, which runs from this server's address and is therefore
// both a weaker claim and a resource somebody else could spend.
const (
	// tryPromptLimit caps what a visitor may send. Long enough to ask a real
	// question, short enough that this route is worthless as free inference.
	tryPromptLimit = 240
	// tryMaxTokens is the answer budget. Bigger than the prober's 32 so the
	// reply is readable, small enough to stay a demonstration.
	tryMaxTokens = 96
	// tryTimeout is shorter than the prober's: a visitor is watching this one.
	tryTimeout = 20 * time.Second
	// tryAnswerLimit is how much of the answer is shown and stored. The point is
	// that the endpoint answered, not what it said.
	tryAnswerLimit = 600
)

// Rate limits. Two of them, because they stop two different things. The
// per-visitor limit stops one person hammering a provider through us. The
// global limit stops a page elsewhere embedding this route and turning every
// one of its readers into a distinct visitor spending our IP's welcome at a
// provider -- which would cost this project the one asset it cannot replace.
const (
	tryVisitorLimit  = 8
	tryVisitorWindow = 10 * time.Minute
	tryGlobalLimit   = 30
	tryGlobalWindow  = time.Minute
)

// TryResult is one visitor-triggered call, recorded the same way the prober's
// own attempts are: what happened, and when. From names whose address made the
// call, and is never blank -- a result whose origin is unstated is exactly the
// claim this site exists to stop making.
type TryResult struct {
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	// URL is the address that was called, so the page can hand back a curl line
	// the visitor can run themselves. Somebody who does not believe the result
	// should not have to reconstruct the request to check it.
	URL        string    `json:"url,omitempty"`
	Prompt     string    `json:"prompt"`
	CalledAt   time.Time `json:"called_at"`
	From       string    `json:"called_from"`
	Outcome    string    `json:"outcome"`
	HTTPStatus int       `json:"http_status"`
	LatencyMS  int       `json:"latency_ms"`
	Answer     string    `json:"answer,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	// Refused is set when we declined to make the call at all. It is separate
	// from an outcome because "we did not ask" and "they said no" are different
	// facts, and merging them would put a failure on the provider's record that
	// the provider never had a chance to cause. RefusedBecause is the machine
	// readable half, so an API caller can tell "ask again later" apart from
	// "that request will never work".
	Refused        string `json:"refused,omitempty"`
	RefusedBecause string `json:"refused_because,omitempty"`
}

// Reasons a call was not made, and the HTTP status each one deserves. A rate
// limit is worth retrying and a request for something we do not track is not.
const (
	RefusedUnknownPair = "unknown_pair"
	RefusedCrawler     = "crawler"
	RefusedRateLimit   = "rate_limited"
	// RefusedNeedsKey covers the second shelf. A test call against a keyed
	// endpoint could only be made on our credential, which would be free
	// inference on our quota handed to whoever asked -- and would answer a
	// question nobody has: that our key works is exactly what the shelf already
	// says. It will never work, so it is a 400 rather than a 429.
	RefusedNeedsKey = "needs_key"
)

// RefusalStatus is the status code for this result, or 200 when a call was
// actually made -- whatever the provider then answered.
func (result TryResult) RefusalStatus() int {
	switch result.RefusedBecause {
	case RefusedUnknownPair, RefusedNeedsKey:
		return http.StatusBadRequest
	case RefusedCrawler:
		return http.StatusForbidden
	case RefusedRateLimit:
		return http.StatusTooManyRequests
	default:
		return http.StatusOK
	}
}

// FromServer is the label on every result this file produces. The browser path
// writes its own, and the two must never be confused for each other.
const FromServer = "stillworks' own server address"

// keyedCallRefusal is the guard that keeps the test-call route off the second
// shelf. It is a free function so it can be tested without a database: Run
// needs one to look the pair up first, and a guard that only the live route
// exercises is a guard that can be deleted in a refactor with every test still
// green.
func keyedCallRefusal(endpoint Endpoint) (string, bool) {
	if !endpoint.RequiresKey() {
		return "", false
	}
	return "That endpoint only answers a request carrying a key. This site holds its own free-tier key " +
		"and probes with it, but it will not spend that key on a call for somebody else — and it would prove " +
		"nothing you cannot read off the shelf. Get your own free key from the provider and run the snippet.", true
}

// Made reports whether a call actually happened, so the view can show a refusal
// as a refusal instead of dressing it as a probe result.
func (result TryResult) Made() bool { return result.Refused == "" }

// TryRunner performs visitor-triggered calls under the limits above. It holds
// the rate limiters, so there is exactly one of it per process.
type TryRunner struct {
	prober   *Prober
	recorder *Recorder
	visitors *rateLimiter
	global   *rateLimiter
}

func NewTryRunner(recorder *Recorder) *TryRunner {
	prober := NewProber()
	prober.Client.Timeout = tryTimeout
	return &TryRunner{
		prober:   prober,
		recorder: recorder,
		visitors: newRateLimiter(tryVisitorLimit, tryVisitorWindow),
		global:   newRateLimiter(tryGlobalLimit, tryGlobalWindow),
	}
}

// Run makes one call on a visitor's behalf, or explains why it did not. It
// never returns an error for a refusal: a refusal is a result the visitor is
// owed an explanation for, not an internal fault.
func (runner *TryRunner) Run(
	ctx context.Context,
	db *database.DB,
	request *http.Request,
	slug string,
	modelID string,
	prompt string,
) (TryResult, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = "Say hello in five words."
	}
	if len(prompt) > tryPromptLimit {
		prompt = prompt[:tryPromptLimit]
	}
	result := TryResult{
		Endpoint: slug,
		Model:    modelID,
		Prompt:   prompt,
		CalledAt: time.Now().UTC(),
		From:     FromServer,
	}

	endpoint, _, tracked, err := TrackedModel(ctx, db, slug, modelID)
	if err != nil {
		return result, err
	}
	if !tracked {
		// Refusing anything not already in the database is what keeps this from
		// being an open relay: the set of callable URLs is the set the prober
		// put there, not the set a visitor can name.
		result.Refused = "That endpoint and model pair is not one this site tracks, so it is not one this site will call."
		result.RefusedBecause = RefusedUnknownPair
		return result, nil
	}
	if reason, refused := keyedCallRefusal(endpoint); refused {
		result.Refused = reason
		result.RefusedBecause = RefusedNeedsKey
		return result, nil
	}
	if isCrawler(clip(request.UserAgent(), userAgentLimit)) {
		// A crawler following this from a link would spend somebody else's free
		// quota on nobody's behalf.
		result.Refused = "Test calls are made for people, not crawlers. Nothing was sent."
		result.RefusedBecause = RefusedCrawler
		return result, nil
	}
	if allowed, retryIn := runner.global.allow("*"); !allowed {
		result.Refused = "This site is already making as many test calls as it is willing to make from its own address " +
			"this minute. Try again in " + humanDelay(retryIn) + ", or run the call from your own machine with the snippet above."
		result.RefusedBecause = RefusedRateLimit
		return result, nil
	}
	if allowed, retryIn := runner.visitors.allow(runner.visitorKey(request)); !allowed {
		result.Refused = "You have used this site's test-call allowance for now. It resets in " + humanDelay(retryIn) + "."
		result.RefusedBecause = RefusedRateLimit
		return result, nil
	}

	started := time.Now()
	body, err := tryBody(endpoint, modelID, prompt)
	if err != nil {
		return result, err
	}
	result.URL = joinURL(endpoint.BaseURL, endpoint.ChatPath)
	status, payload, callErr := runner.prober.doUnauthenticated(ctx, http.MethodPost, result.URL, body)
	result.CalledAt = started.UTC()
	result.LatencyMS = int(time.Since(started).Milliseconds())
	result.HTTPStatus = status
	result.Outcome = classify(status, payload, callErr)
	if callErr != nil {
		result.Detail = callErr.Error()
		return result, nil
	}
	if result.Outcome != OutcomeOK {
		result.Detail = reasonFrom(payload)
		return result, nil
	}
	answer, parseErr := parseCompletion(payload)
	switch {
	case parseErr != nil:
		result.Outcome = OutcomeBadBody
		result.Detail = parseErr.Error()
	case !answer.Served:
		result.Outcome = OutcomeBadBody
		result.Detail = "200 with no completion in the body"
	default:
		result.Answer = truncate(answer.Content, tryAnswerLimit)
	}
	return result, nil
}

// The result of a test call is deliberately not written to llm_probes. That
// table is the site's evidence, produced on a fixed cadence with a fixed prompt;
// letting visitors add rows to it would let anyone move the reliability numbers
// this site publishes.

// visitorKey identifies who is asking, reusing the traffic recorder's daily
// hash rather than inventing a second identity for the same person. When there
// is no recorder -- a management process, or a test -- the address stands in.
func (runner *TryRunner) visitorKey(request *http.Request) string {
	if runner.recorder == nil {
		return clientIP(request)
	}
	return runner.recorder.visitorHash(
		clientIP(request),
		clip(request.UserAgent(), userAgentLimit),
	)
}

func tryBody(endpoint Endpoint, model, prompt string) ([]byte, error) {
	if endpoint.OpenAICompatible {
		return json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": prompt}},
			"max_tokens": tryMaxTokens,
			"stream":     false,
		})
	}
	return json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"stream": false,
	})
}

// rateLimiter is a fixed window per key. Not a token bucket: the thing being
// protected is somebody else's goodwill, and "no more than N in the last
// window" is the rule that is easy to state on the page when it refuses.
type rateLimiter struct {
	mutex  sync.Mutex
	limit  int
	window time.Duration
	seen   map[string][]time.Time
	// now is injected so the limits are testable without sleeping through a
	// ten-minute window.
	now func() time.Time
}

// limiterKeyCeiling bounds the map. Reached only under a flood, at which point
// dropping the oldest state costs an attacker one extra allowed call and costs
// this process nothing.
const limiterKeyCeiling = 4096

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: window,
		seen:   make(map[string][]time.Time),
		now:    time.Now,
	}
}

// allow records an attempt and reports whether it may proceed, plus how long
// until the oldest one in the window expires.
func (limiter *rateLimiter) allow(key string) (bool, time.Duration) {
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()

	now := limiter.now()
	cutoff := now.Add(-limiter.window)
	kept := limiter.seen[key][:0]
	for _, at := range limiter.seen[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= limiter.limit {
		limiter.seen[key] = kept
		return false, limiter.window - now.Sub(kept[0])
	}
	if len(limiter.seen) >= limiterKeyCeiling {
		limiter.sweep(cutoff)
	}
	limiter.seen[key] = append(kept, now)
	return true, 0
}

func (limiter *rateLimiter) sweep(cutoff time.Time) {
	for key, attempts := range limiter.seen {
		if len(attempts) == 0 || !attempts[len(attempts)-1].After(cutoff) {
			delete(limiter.seen, key)
		}
	}
}

// humanDelay rounds up. Telling somebody to come back in "2 minutes" when the
// window clears in 2 minutes and 40 seconds earns a second refusal.
func humanDelay(remaining time.Duration) string {
	if remaining < time.Minute {
		return "under a minute"
	}
	minutes := int(remaining.Minutes()) + 1
	if minutes == 1 {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", minutes)
}

// TrySnippet reproduces exactly the request that was just made, as a shell
// command, so somebody who does not believe the result can run it from their own
// address rather than reconstruct it. It rides on the API's answer because the
// answer is the thing most likely to be disbelieved: this server's address is
// not the caller's, and a per-IP quota is the whole reason the difference
// matters.
//
// Both interpolations are escaped. The command goes to a clipboard and from
// there into somebody else's shell, and a prompt is a visitor's own text.
func TrySnippet(result TryResult) string {
	return fmt.Sprintf(`curl %s \
  -H 'Content-Type: application/json' \
  -d '{"model":"%s","messages":[{"role":"user","content":"%s"}],"max_tokens":%d}'`,
		shellQuote(result.URL), result.Model, escapeForShellJSON(result.Prompt), tryMaxTokens)
}

// shellQuote keeps a URL a single argument. It is not reachable from a hostile
// value today -- base_url and chat_path are only ever written from the in-repo
// seed list, never from a provider's response -- but "not reachable today" is
// one bad seed row away from running a command on a reader's machine, and
// quoting costs nothing.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// escapeForShellJSON keeps a pasted prompt from breaking out of either the JSON
// string or the surrounding single-quoted shell argument.
func escapeForShellJSON(prompt string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "'", `'"'"'`, "\n", " ")
	return replacer.Replace(prompt)
}
