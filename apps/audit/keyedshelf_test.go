package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// keysFrom builds a key reader over a fixed table, so the keyed path is
// exercised without a secret in the test binary's environment.
func keysFrom(table map[string]string) func(string) string {
	return func(name string) string { return table[name] }
}

func keylessEndpoint() Endpoint {
	return Endpoint{
		Slug:             "llm7",
		AuthMode:         AuthModeNone,
		ChatPath:         "/v1/chat/completions",
		ModelsPath:       "/v1/models",
		OpenAICompatible: true,
	}
}

func keyedEndpoint() Endpoint {
	return Endpoint{
		Slug:             "groq",
		AuthMode:         AuthModeKey,
		KeyEnv:           "GROQ_API_KEY",
		ChatPath:         "/openai/v1/chat/completions",
		ModelsPath:       "/openai/v1/models",
		OpenAICompatible: true,
	}
}

// The whole first shelf rests on this one fact about our own code: a keyless
// probe carries no credential. Asserting it at the wire is the only version of
// the claim worth having -- every other check is a check of what we intended.
func TestKeylessProbeSendsNoAuthorizationHeaderEvenWhenWeHoldKeys(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer server.Close()

	prober := NewProber()
	// Deliberately loaded: a key for a *different* provider must not leak onto
	// a keyless endpoint just because the process happens to hold one.
	prober.Keys = keysFrom(map[string]string{"GROQ_API_KEY": "gsk-secret"})
	endpoint := keylessEndpoint()
	endpoint.BaseURL = server.URL

	if probe := prober.ProbeChat(context.Background(), endpoint, "gpt-oss:20b"); probe.Outcome != OutcomeOK {
		t.Fatalf("probe outcome = %q, want ok", probe.Outcome)
	}
	if header := seen.Get("Authorization"); header != "" {
		t.Fatalf("keyless probe sent Authorization %q; the first shelf's entire claim is that it does not", header)
	}
}

func TestKeyedProbeSendsOurKeyAsABearerToken(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer server.Close()

	prober := NewProber()
	prober.Keys = keysFrom(map[string]string{"GROQ_API_KEY": "gsk-secret"})
	endpoint := keyedEndpoint()
	endpoint.BaseURL = server.URL

	if probe := prober.ProbeChat(context.Background(), endpoint, "llama-3.3-70b"); probe.Outcome != OutcomeOK {
		t.Fatalf("probe outcome = %q, want ok", probe.Outcome)
	}
	if got, want := seen.Get("Authorization"), "Bearer gsk-secret"; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

// A key we do not hold means the endpoint is not probed at all. Not probed is
// not "down": recording a 401 we provoked ourselves would publish an outage
// that belongs to our deployment, against somebody else's service.
func TestMissingKeyMeansNotProbedRatherThanFailed(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	prober := NewProber()
	prober.Keys = keysFrom(map[string]string{}) // nothing configured
	endpoint := keyedEndpoint()
	endpoint.BaseURL = server.URL

	if prober.CanProbe(endpoint) {
		t.Fatal("CanProbe said yes for a keyed endpoint whose key is unset")
	}
	if _, held := prober.KeyFor(endpoint); held {
		t.Fatal("KeyFor reported a key it does not have")
	}
	if calls != 0 {
		t.Fatalf("the guard was consulted but %d request(s) went out anyway", calls)
	}
	// Both cycles gate on CanProbe, so the endpoint contributes no probe row,
	// no outcome, and no gap in anybody's reliability record.
	if prober.authorization(endpoint) != "" {
		t.Fatal("an endpoint with no key resolved to a non-empty Authorization header")
	}
}

func TestKeylessEndpointNeverResolvesAKeyEvenIfOneIsNamed(t *testing.T) {
	prober := NewProber()
	prober.Keys = keysFrom(map[string]string{"GROQ_API_KEY": "gsk-secret"})
	// A row that says keyless but carries a key variable is a seeding mistake.
	// It must fail closed -- towards sending nothing -- because the alternative
	// is a keyless verdict earned with a credential.
	confused := keylessEndpoint()
	confused.KeyEnv = "GROQ_API_KEY"

	if _, held := prober.KeyFor(confused); held {
		t.Fatal("a keyless endpoint resolved a key")
	}
	if prober.authorization(confused) != "" {
		t.Fatal("a keyless endpoint produced an Authorization header")
	}
	if !prober.CanProbe(confused) {
		t.Fatal("a keyless endpoint must always be probeable; it needs nothing")
	}
}

func TestBlankKeyIsTreatedAsNoKey(t *testing.T) {
	prober := NewProber()
	prober.Keys = keysFrom(map[string]string{"GROQ_API_KEY": "   "})
	if prober.CanProbe(keyedEndpoint()) {
		t.Fatal("whitespace in the environment variable was accepted as a key")
	}
}

// The verdict column is the rule the second shelf rests on. A keyed probe that
// could write keyless would put key-gated models under a heading promising
// they need none, and nothing in the rendered output would look wrong.
func TestVerdictColumnKeepsTheTwoClaimsApart(t *testing.T) {
	if got := keylessEndpoint().VerdictColumn(); got != "keyless" {
		t.Fatalf("keyless endpoint writes %q", got)
	}
	if got := keyedEndpoint().VerdictColumn(); got != "answered_with_key" {
		t.Fatalf("keyed endpoint writes %q", got)
	}
	// An unrecognised mode falls back to the keyless column, which cannot be
	// reached anyway: the database constrains auth_mode to the two known values.
	if got := (Endpoint{AuthMode: "sometime"}).VerdictColumn(); got != "keyless" {
		t.Fatalf("unknown mode writes %q", got)
	}
}

func TestModelVerifiedReadsTheColumnThatMatchesTheCall(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		endpoint Endpoint
		model    Model
		want     bool
	}{
		{"keyless endpoint, keyless model", keylessEndpoint(), Model{Keyless: &yes}, true},
		{"keyless endpoint, key-only model", keylessEndpoint(), Model{Keyless: &no}, false},
		{"keyless endpoint ignores the keyed column", keylessEndpoint(), Model{AnsweredWithKey: &yes}, false},
		{"keyed endpoint, our key accepted", keyedEndpoint(), Model{AnsweredWithKey: &yes}, true},
		{"keyed endpoint, our key refused", keyedEndpoint(), Model{AnsweredWithKey: &no}, false},
		{"keyed endpoint ignores the keyless column", keyedEndpoint(), Model{Keyless: &yes}, false},
		{"never asked is never a yes", keyedEndpoint(), Model{}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.model.Verified(testCase.endpoint); got != testCase.want {
				t.Fatalf("Verified() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// The SQL fragments are the other half of the same rule, and they are strings,
// so a refactor can break them without the compiler noticing.
func TestShelfConditionsNeverLetOneShelfSatisfyTheOther(t *testing.T) {
	if got := verifiedCondition(AuthModeNone); strings.Contains(got, "answered_with_key") {
		t.Fatalf("keyless filter reads the keyed column: %q", got)
	}
	if got := verifiedCondition(AuthModeKey); strings.Contains(got, "m.keyless") {
		t.Fatalf("keyed filter reads the keyless column: %q", got)
	}
	if got := authCondition(AuthModeNone); got != "e.auth_mode = 'none'" {
		t.Fatalf("keyless shelf condition = %q", got)
	}
	if got := authCondition(AuthModeKey); got != "e.auth_mode = 'key'" {
		t.Fatalf("keyed shelf condition = %q", got)
	}
	// Anything unrecognised narrows to keyless: a mistake yields the stronger
	// claim rather than the wider one.
	if got := authCondition("nonsense"); got != "e.auth_mode = 'none'" {
		t.Fatalf("unknown mode widened the shelf: %q", got)
	}
	if got := verifiedCondition("nonsense"); got != "m.keyless IS TRUE" {
		t.Fatalf("unknown mode widened the verified test: %q", got)
	}
}

// /api/llm/up predates the second shelf. A caller that wrote the bare URL into
// its code has "no key needed" as an assumption, and mixing keyed rows into the
// default answer would hand it a base URL that 401s in production.
func TestRuntimeAPIDefaultsToKeylessAndOnlyWidensWhenAsked(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"no parameters at all", "", AuthModeNone},
		{"only a feature filter", "feature=tools", AuthModeNone},
		{"an empty auth parameter", "auth=", AuthModeNone},
		{"keyless asked for by name", "auth=none", AuthModeNone},
		{"opting in to the keyed shelf", "auth=key", AuthModeKey},
		{"opting in to both", "auth=any", AuthModeAny},
		{"case is not a trap", "auth=KEY", AuthModeKey},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			values, err := url.ParseQuery(testCase.query)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			got, err := requestedAuthMode(values)
			if err != nil {
				t.Fatalf("requestedAuthMode: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("auth = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestUnknownAuthModeIsRefusedRatherThanQuietlyIgnored(t *testing.T) {
	if _, err := requestedAuthMode(url.Values{"auth": []string{"keyed"}}); err == nil {
		t.Fatal("a mistyped auth mode was accepted; the caller would never learn its filter did nothing")
	}
}

// A row can be copied out of a payload and pasted somewhere else. The qualifier
// therefore rides on the row, not only on the envelope.
func TestKeyedRowsCarryTheirQualifierAndKeylessRowsDoNot(t *testing.T) {
	keyed, err := json.Marshal(WorkingModel{
		Slug: "groq", AuthMode: AuthModeKey, ModelID: "llama-3.3-70b",
		BaseURL: "https://api.groq.com", ChatPath: "/openai/v1/chat/completions",
		OpenAICompatible: true,
	})
	if err != nil {
		t.Fatalf("marshal keyed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(keyed, &decoded); err != nil {
		t.Fatalf("unmarshal keyed: %v", err)
	}
	if decoded["auth_mode"] != AuthModeKey {
		t.Fatalf("auth_mode = %v, want %q", decoded["auth_mode"], AuthModeKey)
	}
	note, present := decoded["key_note"].(string)
	if !present || note == "" {
		t.Fatal("a key-required row shipped without saying so")
	}
	if !strings.Contains(note, "not a statement about what a new signup") {
		t.Fatalf("the qualifier does not disclaim the thing a reader will assume: %q", note)
	}

	keyless, err := json.Marshal(WorkingModel{
		Slug: "llm7", AuthMode: AuthModeNone, ModelID: "gpt-oss:20b",
		BaseURL: "https://api.llm7.io", ChatPath: "/v1/chat/completions",
		OpenAICompatible: true,
	})
	if err != nil {
		t.Fatalf("marshal keyless: %v", err)
	}
	if strings.Contains(string(keyless), "key_note") {
		t.Fatalf("a keyless row carried a key qualifier: %s", keyless)
	}
	if !strings.Contains(string(keyless), `"auth_mode": "none"`) &&
		!strings.Contains(string(keyless), `"auth_mode":"none"`) {
		t.Fatalf("a keyless row did not state its auth mode: %s", keyless)
	}
}

// The probe log is public, so anything a provider echoes back at us is
// published. A key that came home inside a 401 body must not survive the trip.
func TestRecordedErrorsNeverCarryTheKeyBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Not hypothetical enough to ignore: an API that quotes the offending
		// credential in its own error message is a real, if rare, thing.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key ` + r.Header.Get("Authorization") + `"}`))
	}))
	defer server.Close()

	prober := NewProber()
	prober.Keys = keysFrom(map[string]string{"GROQ_API_KEY": "gsk-verysecret"})
	endpoint := keyedEndpoint()
	endpoint.BaseURL = server.URL

	probe := prober.ProbeChat(context.Background(), endpoint, "llama-3.3-70b")
	if probe.Outcome != OutcomeNeedsKey {
		t.Fatalf("outcome = %q, want needs_key", probe.Outcome)
	}
	if strings.Contains(probe.Error, "gsk-verysecret") {
		t.Fatalf("the recorded error would publish our key: %q", probe.Error)
	}
	if !strings.Contains(probe.Error, "[redacted]") {
		t.Fatalf("the key was neither kept nor visibly removed: %q", probe.Error)
	}
}

func TestRedactKeyLeavesEverythingElseAlone(t *testing.T) {
	if got := redactKey("nothing to hide", ""); got != "nothing to hide" {
		t.Fatalf("redaction with no key changed the text: %q", got)
	}
	if got := redactKey("", "gsk-secret"); got != "" {
		t.Fatalf("redaction invented text: %q", got)
	}
	if got := redactKey("bad key gsk-secret sent twice: gsk-secret", "gsk-secret"); strings.Contains(got, "gsk-secret") {
		t.Fatalf("a repeated key survived redaction: %q", got)
	}
}

// A path column may hold an absolute URL, and an absolute URL can name any host
// at all. One careless seed edit must not be able to post our key to a stranger.
func TestTheKeyOnlyEverTravelsToTheEndpointsOwnHost(t *testing.T) {
	var elsewhere http.Header
	stranger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = r.Header.Clone()
		_, _ = w.Write([]byte(`{"data":[{"id":"whatever"}]}`))
	}))
	defer stranger.Close()

	prober := NewProber()
	prober.Keys = keysFrom(map[string]string{"GROQ_API_KEY": "gsk-secret"})
	endpoint := keyedEndpoint()
	endpoint.BaseURL = "https://api.groq.com"
	// The shape Pollinations and OpenRouter already use: an absolute path,
	// here pointed at somebody else entirely.
	endpoint.ModelsPath = stranger.URL + "/v1/models"

	probe, _ := prober.ProbeModels(context.Background(), endpoint)
	if probe.Outcome != OutcomeOK {
		t.Fatalf("outcome = %q, want ok: the probe should still happen, just bare", probe.Outcome)
	}
	if header := elsewhere.Get("Authorization"); header != "" {
		t.Fatalf("our key was sent to a host the endpoint does not name: %q", header)
	}
	// And the same-host case still authenticates, or the guard would have
	// silently switched the whole shelf off.
	if prober.credentialFor(endpoint, "https://api.groq.com/openai/v1/models") != "Bearer gsk-secret" {
		t.Fatal("the key stopped travelling to the endpoint's own host")
	}
	if prober.credentialFor(endpoint, "https://API.GROQ.COM/openai/v1/models") != "Bearer gsk-secret" {
		t.Fatal("host comparison is case sensitive; DNS is not")
	}
}

// The test-call button spends our address on a visitor's behalf. On the keyed
// shelf it would spend our credential, which is a different thing entirely.
func TestKeyedEndpointsAreNotTestCallable(t *testing.T) {
	if _, refused := keyedCallRefusal(keylessEndpoint()); refused {
		t.Fatal("a keyless endpoint was refused a test call")
	}
	reason, refused := keyedCallRefusal(keyedEndpoint())
	if !refused {
		t.Fatal("a keyed endpoint was accepted for a test call, which could only be made on our key")
	}
	if !strings.Contains(reason, "will not spend that key") {
		t.Fatalf("the refusal does not say why: %q", reason)
	}
	refusal := TryResult{Refused: reason, RefusedBecause: RefusedNeedsKey}
	if got := refusal.RefusalStatus(); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: a call that can never work is not a retry", got)
	}
	if refusal.Made() {
		t.Fatal("a refusal reported itself as a completed call")
	}
	// And the request path a test call uses cannot authenticate at all: it
	// takes no endpoint, so there is nothing for a key to be resolved from.
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer server.Close()
	prober := NewProber()
	prober.Keys = keysFrom(map[string]string{"GROQ_API_KEY": "gsk-secret"})
	if _, _, err := prober.doUnauthenticated(
		context.Background(), http.MethodPost, server.URL, []byte(`{}`)); err != nil {
		t.Fatalf("doUnauthenticated: %v", err)
	}
	if header := seen.Get("Authorization"); header != "" {
		t.Fatalf("the visitor path sent Authorization %q", header)
	}
}

// Every control on the shelf points back at the shelf, carrying the state it
// was holding. A control that dropped a filter would silently widen the table
// under somebody who had just narrowed it, and one that pointed anywhere else
// would answer a different question from the one that was asked.
func TestShelfControlsStayOnTheShelfAndCarryTheirState(t *testing.T) {
	query := ParseShelfQuery(url.Values{
		searchParameter:  []string{"llama"},
		featureParameter: []string{CapabilityTools},
		keyParameter:     []string{AuthModeKey},
	}).On(shelfPath)

	for name, link := range map[string]string{
		"shelf":   query.URL(),
		"sort":    query.WorkingSortLink("latency"),
		"open":    query.OpenLink("groq", "llama-3.1-8b-instant"),
		"key":     query.KeyLink(AuthModeNone),
		"feature": query.FeatureLink(CapabilityVision),
	} {
		if !strings.HasPrefix(link, shelfPath) {
			t.Errorf("%s link = %q, want a %s prefix", name, link, shelfPath)
		}
		if strings.Contains(link, "/llm/") {
			t.Errorf("%s link points at a retired shelf: %q", name, link)
		}
		// The search survives every one of them. The key filter deliberately
		// does not survive its own chip, and the feature filter does not survive
		// its own chip, which is what makes those chips toggles.
		if parsed := parseLink(t, link); parsed.Get(searchParameter) != "llama" {
			t.Errorf("%s link lost the search: %q", name, link)
		}
	}

	// Opening a row is state, not a filter: it rides in the URL so a link to one
	// model's snippet survives being pasted somewhere, and clicking the same
	// caret again closes it.
	opened := ParseShelfQuery(parseLink(t, query.OpenLink("groq", "gpt-oss:20b")))
	if slug, model := opened.SplitOpen(); slug != "groq" || model != "gpt-oss:20b" {
		t.Fatalf("open round-tripped as %q/%q, want groq/gpt-oss:20b", slug, model)
	}
	if closed := opened.OpenLink("groq", "gpt-oss:20b"); strings.Contains(closed, openParameter+"=") {
		t.Fatalf("clicking the open row again did not close it: %q", closed)
	}

	// A key chip clears the open row rather than carrying it: a drawer left open
	// on a row the new filter does not render is a parameter that does nothing.
	if link := query.KeyLink(AuthModeNone); strings.Contains(link, openParameter+"=") {
		t.Fatalf("the key chip carried an open row into a different filter: %q", link)
	}
}

// The policy exists to let the test-call button reach a provider from the
// visitor's browser. There is no such button on the keyed shelf, so listing
// those hosts would grant a reach nothing uses.
func TestBrowserCallOriginsExcludeKeyedProviders(t *testing.T) {
	origins := BrowserCallOrigins()
	for _, endpoint := range SeedEndpoints {
		if !endpoint.RequiresKey() {
			continue
		}
		parsed, err := url.Parse(endpoint.BaseURL)
		if err != nil {
			t.Fatalf("seed %s has an unparseable base URL: %v", endpoint.Slug, err)
		}
		origin := parsed.Scheme + "://" + parsed.Host
		for _, allowed := range origins {
			if allowed == origin {
				t.Fatalf("keyed provider %s is in the browser call policy", endpoint.Slug)
			}
		}
	}
}

// Every keyed seed must name the variable its key lives in, and no keyless seed
// may name one. The first is what makes an endpoint probeable; the second is
// what would make a keyless verdict suspect.
func TestSeededEndpointsDeclareTheirCredentialsHonestly(t *testing.T) {
	for _, endpoint := range SeedEndpoints {
		switch endpoint.AuthMode {
		case AuthModeKey:
			if endpoint.KeyEnv == "" {
				t.Errorf("%s is keyed but names no environment variable, so it can never be probed",
					endpoint.Slug)
			}
			if !strings.HasSuffix(endpoint.KeyEnv, "_API_KEY") {
				t.Errorf("%s reads %q; keep the naming uniform so a deploy note can be read at a glance",
					endpoint.Slug, endpoint.KeyEnv)
			}
		case AuthModeNone:
			if endpoint.KeyEnv != "" {
				t.Errorf("%s claims to be keyless but names key variable %q",
					endpoint.Slug, endpoint.KeyEnv)
			}
		default:
			t.Errorf("%s has auth mode %q, which the database will refuse", endpoint.Slug, endpoint.AuthMode)
		}
		if strings.TrimSpace(endpoint.Notes) == "" {
			t.Errorf("%s carries no note saying where its claim came from", endpoint.Slug)
		}
	}
}

// reservedSlugs are the path segments under /llm/ that are pages rather than
// endpoints. They are registered before /llm/{slug}, so an endpoint named after
// one would be listed on the shelf with a link that renders a different page --
// silently, and only for that one row.
var reservedSlugs = []string{"try", "keyed"}

func TestNoSeededSlugCollidesWithAPage(t *testing.T) {
	for _, endpoint := range SeedEndpoints {
		for _, reserved := range reservedSlugs {
			if endpoint.Slug == reserved {
				t.Errorf("endpoint %s is named after the /llm/%s page, so its own page is unreachable",
					endpoint.Slug, reserved)
			}
		}
	}
}

// Ordering the per-endpoint model table by the wrong verdict column would put
// models this shelf has never checked above ones it has. seedllm updates
// auth_mode in place, so a row really can carry a stale verdict from the shelf
// it used to be on.
func TestModelTableOrdersByTheColumnItsShelfWrites(t *testing.T) {
	if got := verdictColumnFor(AuthModeNone); got != "keyless" {
		t.Fatalf("keyless shelf orders by %q", got)
	}
	if got := verdictColumnFor(AuthModeKey); got != "answered_with_key" {
		t.Fatalf("keyed shelf orders by %q", got)
	}
	// The read side and the write side have to name the same column, or a shelf
	// could filter on one and sort by the other.
	if verdictColumnFor(AuthModeKey) != keyedEndpoint().VerdictColumn() {
		t.Fatal("the read and write sides disagree about the keyed verdict column")
	}
	if verdictColumnFor(AuthModeNone) != keylessEndpoint().VerdictColumn() {
		t.Fatal("the read and write sides disagree about the keyless verdict column")
	}
}

// The .env output is a file people paste into a project. On the keyed shelf it
// must not look complete, and it must never contain our key.
func TestEnvOutputNeverHandsOutOurKey(t *testing.T) {
	response := httptest.NewRecorder()
	writeEnv(response, []WorkingModel{{
		Slug: "groq", AuthMode: AuthModeKey, ModelID: "llama-3.3-70b",
		BaseURL: "https://api.groq.com", ChatPath: "/openai/v1/chat/completions",
		OpenAICompatible: true, ChatCapable: true,
	}})
	body := response.Body.String()
	if !strings.Contains(body, "OPENAI_API_KEY=your-groq-api-key") {
		t.Fatalf("keyed .env did not ask the reader for their own key:\n%s", body)
	}
	if strings.Contains(body, "not-needed") {
		t.Fatalf("keyed .env told the reader no key is needed:\n%s", body)
	}
	if !strings.Contains(body, "requires an API key") {
		t.Fatalf("keyed .env did not say a key is required:\n%s", body)
	}

	response = httptest.NewRecorder()
	writeEnv(response, []WorkingModel{{
		Slug: "llm7", AuthMode: AuthModeNone, ModelID: "gpt-oss:20b",
		BaseURL: "https://api.llm7.io", ChatPath: "/v1/chat/completions",
		OpenAICompatible: true, ChatCapable: true,
	}})
	if body := response.Body.String(); !strings.Contains(body, "OPENAI_API_KEY=not-needed") {
		t.Fatalf("keyless .env stopped saying no key is needed:\n%s", body)
	}
}

// A keyed snippet that looked runnable would be pasted straight into a project
// and fail on the first call, with nothing on the page to explain why.
func TestKeyedSnippetAsksForTheReadersOwnKey(t *testing.T) {
	keyed := snippet(WorkingModel{
		Slug: "groq", AuthMode: AuthModeKey, ModelID: "llama-3.3-70b",
		BaseURL: "https://api.groq.com", ChatPath: "/openai/v1/chat/completions",
		OpenAICompatible: true, ChatCapable: true,
	})
	if !strings.Contains(keyed, "Authorization: Bearer YOUR_GROQ_API_KEY") {
		t.Fatalf("keyed snippet carries no placeholder credential:\n%s", keyed)
	}
	keyless := snippet(WorkingModel{
		Slug: "llm7", AuthMode: AuthModeNone, ModelID: "gpt-oss:20b",
		BaseURL: "https://api.llm7.io", ChatPath: "/v1/chat/completions",
		OpenAICompatible: true, ChatCapable: true,
	})
	if strings.Contains(keyless, "Authorization") {
		t.Fatalf("keyless snippet grew an Authorization header:\n%s", keyless)
	}
}

func TestCapabilityQuestionsFollowTheEndpointsOwnVerdict(t *testing.T) {
	yes := true
	// A keyed model proven to answer our key is worth asking about its
	// features. Reading the keyless column here would find NULL and ask
	// nothing, leaving the whole keyed shelf without a capability matrix.
	keyedModel := Model{ModelID: "llama-3.3-70b", ChatCapable: true, AnsweredWithKey: &yes}
	if got := applicableCapabilities(keyedEndpoint(), keyedModel); len(got) != len(ChatCapabilities) {
		t.Fatalf("keyed model got %v, want the full chat set", got)
	}
	// And a model whose only "yes" is in the other shelf's column is not asked.
	crossed := Model{ModelID: "llama-3.3-70b", ChatCapable: true, Keyless: &yes}
	if got := applicableCapabilities(keyedEndpoint(), crossed); got != nil {
		t.Fatalf("a keyless verdict qualified a model on the keyed shelf: %v", got)
	}
}

// The shelf's "Paste this" block is built from EnvLines and snippet, the same
// two functions the API's ?format=env output uses. On the keyed shelf both have
// to emit a placeholder: our own free-tier key is never rendered on this site,
// and a block that looked complete would be pasted into a project and fail on
// the first call with nothing on screen to explain why.
//
// This is covered here rather than in a browser because a deployment holding no
// key for a provider has no verified keyed rows to render, so the live page
// cannot reach this branch without a real credential.
func TestThePasteBlockNeverRendersOurOwnKey(t *testing.T) {
	const secret = "gsk_thisisoursandmustneverappear"
	keyed := WorkingModel{
		Slug:             "groq",
		BaseURL:          "https://api.groq.com",
		ChatPath:         "/openai/v1/chat/completions",
		AuthMode:         AuthModeKey,
		ModelID:          "llama-3.1-8b-instant",
		OpenAICompatible: true,
		ChatCapable:      true,
	}
	for name, rendered := range map[string]string{"env": EnvLines(keyed), "curl": snippet(keyed)} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("%s block rendered a real key: %q", name, rendered)
		}
		if !strings.Contains(strings.ToLower(rendered), "your") {
			t.Fatalf("%s block does not mark the key as the reader's to supply: %q", name, rendered)
		}
	}
	// The base URL is the one an OpenAI client can be handed, not the bare
	// host: Groq mounts its OpenAI surface under /openai/v1.
	if !strings.Contains(EnvLines(keyed), "OPENAI_BASE_URL=https://api.groq.com/openai/v1\n") {
		t.Fatalf("keyed env block has the wrong base URL: %q", EnvLines(keyed))
	}

	// The keyless block says the opposite, in the strongest form: not a
	// placeholder, but that no header should be sent at all.
	keyless := keyed
	keyless.AuthMode = AuthModeNone
	keyless.Slug = "pollinations"
	keyless.BaseURL = "https://text.pollinations.ai"
	keyless.ChatPath = "/openai"
	if !strings.Contains(EnvLines(keyless), "OPENAI_API_KEY=not-needed") {
		t.Fatalf("keyless env block should say the key is not needed: %q", EnvLines(keyless))
	}
	if strings.Contains(snippet(keyless), "Authorization") {
		t.Fatalf("keyless snippet must carry no Authorization header at all: %q", snippet(keyless))
	}
}

// EnvLines is what /api/llm/up?format=env emits and what the page prints. One
// function, so the page cannot drift from the API -- the page printing its own
// derivation is what put a base URL on every endpoint page that answers 404.
func TestThePageAndTheAPIShareOneEnvDerivation(t *testing.T) {
	model := WorkingModel{
		Slug:             "ovh-anonymous",
		BaseURL:          "https://oai.endpoints.kepler.ai.cloud.ovh.net",
		ChatPath:         "/v1/chat/completions",
		AuthMode:         AuthModeNone,
		ModelID:          "Mistral-7B-Instruct-v0.3",
		OpenAICompatible: true,
		ChatCapable:      true,
	}
	response := httptest.NewRecorder()
	writeEnv(response, []WorkingModel{model})
	if !strings.Contains(response.Body.String(), EnvLines(model)) {
		t.Fatalf("format=env output does not contain the block the page prints:\napi:  %q\npage: %q",
			response.Body.String(), EnvLines(model))
	}
	// And that block carries the /v1 the provider's own documented host omits.
	if !strings.Contains(EnvLines(model), "kepler.ai.cloud.ovh.net/v1\n") {
		t.Fatalf("base URL lost its version prefix: %q", EnvLines(model))
	}
}

// Which body an endpoint speaks and whether it needs a credential are separate
// questions, and the snippet has to get all four combinations right. Nesting
// them dropped the Authorization line from an endpoint that was both Ollama-
// shaped and key-required -- a snippet that cannot work, on a shelf whose whole
// claim is that what it publishes was actually called.
func TestTheSnippetAsksTheShapeAndTheKeyQuestionsSeparately(t *testing.T) {
	base := WorkingModel{ModelID: "tinyllama", BaseURL: "https://example.test", ChatPath: "/api/generate", Slug: "some-host"}
	openAI := WorkingModel{ModelID: "gpt-oss", BaseURL: "https://example.test", ChatPath: "/v1/chat/completions", Slug: "some-host"}

	cases := []struct {
		name       string
		model      WorkingModel
		wantBody   string
		wantAuth   bool
		rejectBody string
	}{
		{
			name:       "ollama shaped, no key",
			model:      func() WorkingModel { m := base; m.AuthMode = AuthModeNone; return m }(),
			wantBody:   `"prompt":"hello"`,
			rejectBody: `"messages"`,
		},
		{
			name:       "ollama shaped, key required",
			model:      func() WorkingModel { m := base; m.AuthMode = AuthModeKey; return m }(),
			wantBody:   `"prompt":"hello"`,
			wantAuth:   true,
			rejectBody: `"messages"`,
		},
		{
			name:       "openai shaped, no key",
			model:      func() WorkingModel { m := openAI; m.AuthMode = AuthModeNone; m.OpenAICompatible = true; return m }(),
			wantBody:   `"messages"`,
			rejectBody: `"prompt"`,
		},
		{
			name:       "openai shaped, key required",
			model:      func() WorkingModel { m := openAI; m.AuthMode = AuthModeKey; m.OpenAICompatible = true; return m }(),
			wantBody:   `"messages"`,
			wantAuth:   true,
			rejectBody: `"prompt"`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := snippet(testCase.model)
			if !strings.Contains(got, testCase.wantBody) {
				t.Fatalf("snippet body is the wrong shape, want %s:\n%s", testCase.wantBody, got)
			}
			if strings.Contains(got, testCase.rejectBody) {
				t.Fatalf("snippet body carries %s, which this endpoint does not speak:\n%s", testCase.rejectBody, got)
			}
			if hasAuth := strings.Contains(got, "Authorization"); hasAuth != testCase.wantAuth {
				t.Fatalf("Authorization header present=%v, want %v:\n%s", hasAuth, testCase.wantAuth, got)
			}
		})
	}
}
