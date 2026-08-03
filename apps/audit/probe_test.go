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
		{"payment required is a false keyless claim", http.StatusPaymentRequired, nil, nil, OutcomeNeedsKey},
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
