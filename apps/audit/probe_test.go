package audit

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestClassifySeparatesKeyDemandFromDowntime(t *testing.T) {
	// The nginx page is mlvoca.com's actual reply to every request, verified
	// 2026-08-03. It must never be read as a key demand.
	const nginxForbidden = "<html>\n<head><title>403 Forbidden</title></head>\n" +
		"<body>\n<center><h1>403 Forbidden</h1></center>\n<hr><center>nginx/1.22.1</center>\n</body>\n</html>"
	const apiKeyRefusal = `{"error":{"message":"missing_api_key","type":"invalid_request_error"}}`

	cases := []struct {
		name   string
		status int
		body   []byte
		err    error
		want   string
	}{
		{"ok", http.StatusOK, nil, nil, OutcomeOK},
		{"unauthorized is a false keyless claim", http.StatusUnauthorized, nil, nil, OutcomeNeedsKey},
		{"payment required with no explanation", http.StatusPaymentRequired, nil, nil, OutcomeNeedsKey},
		// Verbatim from pollinations on 2026-08-03, returned to a request that
		// carried no key at all; the same endpoint served 200 a minute later.
		{"payment required because the anonymous pool ran dry", http.StatusPaymentRequired,
			[]byte(`{"error":"402 Payment Required","status":402,"details":{"success":false,` +
				`"error":{"message":"API key budget too low. This request requires more credits"}}}`),
			nil, OutcomeRateLimited},
		{"403 from an API asking for a key", http.StatusForbidden, []byte(apiKeyRefusal), nil, OutcomeNeedsKey},
		{"403 from a proxy is blocked, not a key demand", http.StatusForbidden, []byte(nginxForbidden), nil, OutcomeBlocked},
		{"403 with no body is blocked", http.StatusForbidden, nil, nil, OutcomeBlocked},
		{"throttled is alive", http.StatusTooManyRequests, nil, nil, OutcomeRateLimited},
		{"server error", http.StatusBadGateway, nil, nil, OutcomeServerError},
		{"bad request", http.StatusBadRequest, nil, nil, OutcomeClientError},
		{"deadline", 0, nil, context.DeadlineExceeded, OutcomeTimeout},
		{"transport failure", 0, nil, errors.New("dial tcp: no route to host"), OutcomeUnreachable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := classify(testCase.status, testCase.body, testCase.err); got != testCase.want {
				t.Fatalf("classify(%d, %v) = %q, want %q",
					testCase.status, testCase.err, got, testCase.want)
			}
		})
	}
}

// Regression: llm7's gpt-oss:20b answered correctly on 2026-08-03 but spent the
// whole token budget on hidden reasoning, returning content:"" with
// finish_reason:"length". The prober recorded bad_body -- a working keyless
// endpoint reported as broken, which is worse than not listing it at all.
func TestReasoningOnlyAnswerCountsAsServed(t *testing.T) {
	payload := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"",` +
		`"reasoning":"User says \"ping\"."},"finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":68,"completion_tokens":8,"total_tokens":76}}`)
	answer, err := parseCompletion(payload)
	if err != nil {
		t.Fatalf("parseCompletion: %v", err)
	}
	if !answer.Served {
		t.Fatal("reasoning-only response must count as served")
	}
}

func TestTruncatedWithNoTextStillCountsAsServed(t *testing.T) {
	payload := []byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}],` +
		`"usage":{"completion_tokens":32}}`)
	answer, err := parseCompletion(payload)
	if err != nil {
		t.Fatalf("parseCompletion: %v", err)
	}
	if !answer.Served {
		t.Fatal("truncated response with emitted tokens must count as served")
	}
}

func TestEmptyBodyIsNotServed(t *testing.T) {
	payload := []byte(`{"choices":[{"message":{"content":""},"finish_reason":"stop"}],` +
		`"usage":{"completion_tokens":0}}`)
	answer, err := parseCompletion(payload)
	if err != nil {
		t.Fatalf("parseCompletion: %v", err)
	}
	if answer.Served {
		t.Fatal("a 200 with no output at all must not count as served")
	}
}

func TestParseCompletionHandlesOllamaShape(t *testing.T) {
	answer, err := parseCompletion([]byte(`{"response":"pong","done":true}`))
	if err != nil {
		t.Fatalf("parseCompletion: %v", err)
	}
	if !answer.Served || answer.Content != "pong" {
		t.Fatalf("got %+v, want served pong", answer)
	}
}

func TestParseModelsAcceptsTheThreeShapesServedInTheWild(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		wantID  string
	}{
		{"openai envelope", `{"data":[{"id":"gpt-oss:20b","tier":"turbo"}]}`, "gpt-oss:20b"},
		{"bare array", `[{"name":"openai-fast","tier":"anonymous"}]`, "openai-fast"},
		{"ollama tags", `{"models":[{"name":"tinyllama:latest"}]}`, "tinyllama:latest"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			models, err := parseModels([]byte(testCase.payload))
			if err != nil {
				t.Fatalf("parseModels: %v", err)
			}
			if len(models) != 1 || models[0].ID != testCase.wantID {
				t.Fatalf("got %+v, want one model %q", models, testCase.wantID)
			}
		})
	}
}

// Both listings are verbatim from 2026-08-04, trimmed to the fields a claim is
// read from. The claims are recorded so the shelf can show them beside the
// measurement -- llm7 claims tools for every turbo model and json_mode for one
// whose json_schema request comes back 405.
func TestParseModelsRecordsWhatTheProviderClaims(t *testing.T) {
	llm7 := `{"data":[{"id":"gpt-oss:20b","model_type":"chat","tier":"turbo",` +
		`"modalities":{"input":["text"],"output":["text"]},"json_mode":true,"tools_calling":true,` +
		`"capabilities":{"vision":false,"tools":true,"json_mode":true,"reasoning":true}}]}`
	models, err := parseModels([]byte(llm7))
	if err != nil {
		t.Fatalf("parseModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	model := models[0]
	if !model.ChatCapable {
		t.Fatal("model_type chat must settle chat capability")
	}
	if model.InputModes != "text" || model.OutputModes != "text" {
		t.Fatalf("nested modalities were not read: in=%q out=%q", model.InputModes, model.OutputModes)
	}
	if got, want := model.Claims[CapabilityTools], true; got != want {
		t.Fatalf("tools claim = %v, want %v", got, want)
	}
	// vision:false is a claim, not an absence: the map has to carry it so a
	// verified yes can be shown as contradicting the provider.
	claimed, present := model.Claims[CapabilityVision]
	if !present || claimed {
		t.Fatalf("vision claim = %v present=%v, want false and present", claimed, present)
	}
	// json_mode is the weaker mode's name, so it is not read as a json_schema
	// claim. That distinction is the whole reason the two are tracked apart.
	if _, present := model.Claims[CapabilityJSONSchema]; present {
		t.Fatal("json_mode must not be recorded as a json_schema claim")
	}
	if !model.Claims[CapabilityJSONObject] {
		t.Fatal("json_mode must be recorded as a json_object claim")
	}

	pollinations := `[{"name":"openai-fast","tier":"anonymous","input_modalities":["text"],` +
		`"output_modalities":["text"],"tools":true,"vision":false}]`
	models, err = parseModels([]byte(pollinations))
	if err != nil {
		t.Fatalf("parseModels: %v", err)
	}
	if !models[0].Claims[CapabilityTools] {
		t.Fatal("a top-level tools flag is still a claim")
	}
}

// llm7 labels nine of its models image or video. Those are never chat probed,
// and the image ones are the only candidates for an image generation verdict.
func TestModelTypeSettlesWhatAModelIsFor(t *testing.T) {
	payload := `{"data":[{"id":"gpt-image-2","model_type":"image","tier":"pro"},` +
		`{"id":"gemini-veo31","model_type":"video","tier":"pro"}]}`
	models, err := parseModels([]byte(payload))
	if err != nil {
		t.Fatalf("parseModels: %v", err)
	}
	for _, model := range models {
		if model.ChatCapable {
			t.Fatalf("%q is not a chat model", model.ID)
		}
	}
	if !models[0].Claims[CapabilityImageOut] {
		t.Fatal("a model the provider calls an image model claims image output")
	}
	if _, present := models[1].Claims[CapabilityImageOut]; present {
		t.Fatal("a video model does not claim image output")
	}
}

// image.pollinations.ai/models answers ["sana"] and nothing else. Without this
// shape the image endpoint has no model to hang a verdict on.
func TestParseModelsAcceptsABareArrayOfNames(t *testing.T) {
	models, err := parseModels([]byte(`["sana"]`))
	if err != nil {
		t.Fatalf("parseModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "sana" {
		t.Fatalf("got %+v, want one model sana", models)
	}
}

func TestParseModelsRejectsUnrecognisedBody(t *testing.T) {
	if _, err := parseModels([]byte(`{"error":"nope"}`)); err == nil {
		t.Fatal("expected an error for a body carrying no model ids")
	}
}

func TestChatCapablePrefersPublishedModalities(t *testing.T) {
	if !chatCapable("openai-fast", []string{"text"}, []string{"text"}) {
		t.Fatal("text to text must be chat capable")
	}
	// Named like a chat model, but the provider says it emits images.
	if chatCapable("some-chatty-name", []string{"text"}, []string{"image"}) {
		t.Fatal("published modalities must beat the name heuristic")
	}
}

func TestChatCapableFallsBackToTheNameWhenNoModalitiesPublished(t *testing.T) {
	// OVH publishes no modalities and lists these beside its chat models.
	nonChat := []string{
		"stable-diffusion-xl-base-v10",
		"stabilityai/stable-diffusion-xl-base-1.0",
		"Qwen3-Embedding-8B",
		"bge-multilingual-gemma2",
		"whisper-large-v3",
		"gpt-image-2",
	}
	for _, id := range nonChat {
		if chatCapable(id, nil, nil) {
			t.Fatalf("%q must not be chat probed", id)
		}
	}
	chat := []string{
		"Mistral-7B-Instruct-v0.3",
		"Meta-Llama-3_3-70B-Instruct",
		"gpt-oss:20b",
		"codestral-latest",
	}
	for _, id := range chat {
		if !chatCapable(id, nil, nil) {
			t.Fatalf("%q must be chat probed", id)
		}
	}
}

func TestJoinURLKeepsBasePath(t *testing.T) {
	cases := map[string]string{
		joinURL("https://api.llm7.io", "/v1/models"):        "https://api.llm7.io/v1/models",
		joinURL("https://text.pollinations.ai", "/openai"):  "https://text.pollinations.ai/openai",
		joinURL("https://example.test/", "/v1/models"):      "https://example.test/v1/models",
		joinURL("https://example.test", "https://other/go"): "https://other/go",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("joinURL = %q, want %q", got, want)
		}
	}
}

// The raw probe log's Detail column read "{" for every provider that
// pretty-prints its errors -- OVH's rate limiter among them -- because the
// recorded reason was the body's first line. That is the single most useful
// cell on an endpoint page reduced to a brace.
func TestReasonFromDigsTheMessageOutOfAJSONErrorBody(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "pretty printed nested error",
			payload: "{\n  \"error\": {\n    \"message\": \"Rate limit exceeded\",\n    \"code\": 429\n  }\n}",
			want:    "Rate limit exceeded",
		},
		{
			name:    "error as a bare string",
			payload: `{"error":"Queue full for IP: 1 request already queued","status":429}`,
			want:    "Queue full for IP: 1 request already queued",
		},
		{
			name:    "top level message",
			payload: "{\n  \"message\": \"missing_api_key\"\n}",
			want:    "missing_api_key",
		},
		{
			name:    "top level detail",
			payload: "{\n  \"detail\": \"Not Found\"\n}",
			want:    "Not Found",
		},
		{
			// Not JSON at all: an edge serving an HTML refusal. The first line
			// is still the best available answer and is still what we keep.
			name:    "not json falls back to the first line",
			payload: "<html>\n<head><title>403 Forbidden</title></head>",
			want:    "<html>",
		},
		{
			// JSON we cannot find a message in must not become an empty cell.
			name:    "json with no message keeps the first line",
			payload: "{\n  \"code\": 500\n}",
			want:    "{",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := reasonFrom([]byte(testCase.payload)); got != testCase.want {
				t.Fatalf("reasonFrom() = %q, want %q", got, testCase.want)
			}
		})
	}
}
