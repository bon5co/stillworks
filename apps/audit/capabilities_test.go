package audit

import (
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func wantVerdict(t *testing.T, got *bool, want *bool, detail string) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("expected the verdict to be left unchanged, got %v (%s)", *got, detail)
	case want != nil && got == nil:
		t.Fatalf("expected verdict %v, got unchanged (%s)", *want, detail)
	case want != nil && got != nil && *want != *got:
		t.Fatalf("expected verdict %v, got %v (%s)", *want, *got, detail)
	}
}

// The successful payload is llm7's actual reply on 2026-08-04, trimmed to the
// fields a verdict is read from.
func TestToolCallVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    *bool
	}{
		{
			name: "a call the caller could dispatch",
			payload: `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_qq68teee",` +
				`"index":0,"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},` +
				`"finish_reason":"tool_calls"}]}`,
			want: supportedTrue(),
		},
		{
			// The case a status-code check gets wrong: a perfectly good 200
			// carrying prose. Every list that reports "supports tools" from a
			// 200 reports this model as supporting tools.
			name: "a 200 that answered instead of calling",
			payload: `{"choices":[{"message":{"role":"assistant","content":"I do not have live weather data, ` +
				`but Paris is usually mild in August."},"finish_reason":"stop"}]}`,
			want: supportedFalse(),
		},
		{
			name: "the legacy single function_call shape",
			payload: `{"choices":[{"message":{"role":"assistant","function_call":{"name":"get_weather",` +
				`"arguments":"{\"city\": \"Paris\"}"}},"finish_reason":"function_call"}]}`,
			want: supportedTrue(),
		},
		{
			name: "arguments that are prose rather than JSON",
			payload: `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"get_weather",` +
				`"arguments":"the city of Paris"}}]},"finish_reason":"tool_calls"}]}`,
			want: supportedFalse(),
		},
		{
			name: "a tool call with no function name",
			payload: `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"",` +
				`"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			want: supportedFalse(),
		},
		{
			// "null" parses without an error and leaves a nil map, so a check
			// that only looks at the error reads this as working. It breaks a
			// caller's tool loop on the first field read.
			name: "arguments that are the literal null",
			payload: `{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"get_weather",` +
				`"arguments":"null"}}]},"finish_reason":"tool_calls"}]}`,
			want: supportedFalse(),
		},
		{
			// A reasoning model can burn the whole budget before deciding to
			// call anything. That is our stinginess, not its incapacity.
			name:    "cut off at the token budget",
			payload: `{"choices":[{"message":{"content":"Let me think about which"},"finish_reason":"length"}]}`,
			want:    nil,
		},
		{
			name:    "a 200 with no choices at all",
			payload: `{"choices":[]}`,
			want:    nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, detail := verifyToolCall([]byte(testCase.payload))
			wantVerdict(t, got, testCase.want, detail)
		})
	}
}

func TestStructuredOutputVerdicts(t *testing.T) {
	cases := []struct {
		name          string
		payload       string
		requireSchema bool
		want          *bool
	}{
		{
			// llm7's gpt-oss:20b on 2026-08-04, reasoning field trimmed.
			name:          "exactly the schema that was demanded",
			payload:       `{"choices":[{"message":{"content":"{\"city\":\"Paris\",\"country\":\"France\"}"},"finish_reason":"stop"}]}`,
			requireSchema: true,
			want:          supportedTrue(),
		},
		{
			// The failure a "did it return 200" check calls a success.
			name:          "valid JSON wrapped in a markdown fence",
			payload:       "{\"choices\":[{\"message\":{\"content\":\"```json\\n{\\\"city\\\":\\\"Paris\\\"}\\n```\"},\"finish_reason\":\"stop\"}]}",
			requireSchema: true,
			want:          supportedFalse(),
		},
		{
			name:          "prose where an object was demanded",
			payload:       `{"choices":[{"message":{"content":"The capital of France is Paris."},"finish_reason":"stop"}]}`,
			requireSchema: true,
			want:          supportedFalse(),
		},
		{
			name:          "valid JSON that ignores the schema",
			payload:       `{"choices":[{"message":{"content":"{\"capital\":\"Paris\"}"},"finish_reason":"stop"}]}`,
			requireSchema: true,
			want:          supportedFalse(),
		},
		{
			name:          "a required field of the wrong type",
			payload:       `{"choices":[{"message":{"content":"{\"city\":\"Paris\",\"country\":42}"},"finish_reason":"stop"}]}`,
			requireSchema: true,
			want:          supportedFalse(),
		},
		{
			name:          "extra keys the schema forbade",
			payload:       `{"choices":[{"message":{"content":"{\"city\":\"Paris\",\"country\":\"France\",\"note\":\"hi\"}"},"finish_reason":"stop"}]}`,
			requireSchema: true,
			want:          supportedFalse(),
		},
		{
			name:          "json_object mode does not demand the schema",
			payload:       `{"choices":[{"message":{"content":"{\"capital\":\"Paris\"}"},"finish_reason":"stop"}]}`,
			requireSchema: false,
			want:          supportedTrue(),
		},
		{
			// The regression the chat probe already learned once: a reasoning
			// model that spent the budget thinking is not a model that failed.
			name:          "truncated before the JSON closed",
			payload:       `{"choices":[{"message":{"content":"{\"city\":\"Par"},"finish_reason":"length"}]}`,
			requireSchema: true,
			want:          nil,
		},
		{
			name:          "empty content because the budget went on reasoning",
			payload:       `{"choices":[{"message":{"content":"","reasoning":"The user wants JSON."},"finish_reason":"length"}]}`,
			requireSchema: true,
			want:          nil,
		},
		{
			name:          "a completed answer that is simply empty",
			payload:       `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
			requireSchema: false,
			want:          supportedFalse(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, detail := verifyStructuredOutput([]byte(testCase.payload), testCase.requireSchema)
			wantVerdict(t, got, testCase.want, detail)
		})
	}
}

func TestVisionVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    *bool
	}{
		{
			// OVH's Mistral-Small-3.2-24B-Instruct-2506 answered exactly this
			// to the real probe image on 2026-08-04.
			name:    "a one word answer naming the colour we sent",
			payload: `{"choices":[{"message":{"content":"Red"},"finish_reason":"stop"}]}`,
			want:    supportedTrue(),
		},
		{
			// Vocabulary must not decide this: crimson is a correct answer.
			name:    "a different word for the same colour",
			payload: `{"choices":[{"message":{"content":"Crimson."},"finish_reason":"stop"}]}`,
			want:    supportedTrue(),
		},
		{
			// The case a non-refusal check gets wrong: a text-only model that
			// guesses rather than admitting it saw nothing.
			name:    "a confident answer about a colour the image is not",
			payload: `{"choices":[{"message":{"content":"Blue."},"finish_reason":"stop"}]}`,
			want:    supportedFalse(),
		},
		{
			name:    "an answer that names no colour at all",
			payload: `{"choices":[{"message":{"content":"It is a small square."},"finish_reason":"stop"}]}`,
			want:    nil,
		},
		{
			// A model that really looked can mention what the image does not
			// contain. Matching a refusal fragment anywhere in the reply, ahead
			// of the colour, failed exactly this answer.
			name:    "a correct answer that also mentions an absence",
			payload: `{"choices":[{"message":{"content":"Red. There is no image text beyond the fill."},"finish_reason":"stop"}]}`,
			want:    supportedTrue(),
		},
		{
			name:    "an answer in the model's own script",
			payload: `{"choices":[{"message":{"content":"红色"},"finish_reason":"stop"}]}`,
			want:    supportedTrue(),
		},
		{
			name:    "a polite refusal is a verified no",
			payload: `{"choices":[{"message":{"content":"I'm sorry, I cannot see images."},"finish_reason":"stop"}]}`,
			want:    supportedFalse(),
		},
		{
			name:    "a text-only model saying so",
			payload: `{"choices":[{"message":{"content":"As a text-based assistant I have no image here."},"finish_reason":"stop"}]}`,
			want:    supportedFalse(),
		},
		{
			name:    "budget spent on reasoning",
			payload: `{"choices":[{"message":{"content":"","reasoning":"Looking at the picture"},"finish_reason":"length"}]}`,
			want:    nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, detail := verifyVisionAnswer([]byte(testCase.payload))
			wantVerdict(t, got, testCase.want, detail)
		})
	}
}

func TestImageBytesVerdicts(t *testing.T) {
	pngHeader := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 13}
	encoded := base64.StdEncoding.EncodeToString(pngHeader)

	cases := []struct {
		name        string
		contentType string
		payload     string
		want        *bool
	}{
		{
			// Pollinations answers the prompt URL with the picture itself.
			name:        "image content type carrying real image bytes",
			contentType: "image/jpeg",
			payload:     "\xFF\xD8\xFF\xE0 rest of a jpeg",
			want:        supportedTrue(),
		},
		{
			// The check a content-type-only test gets wrong: an edge that
			// labels its error page image/jpeg.
			name:        "image content type carrying something else",
			contentType: "image/jpeg",
			payload:     "<html><body>upstream error</body></html>",
			want:        supportedFalse(),
		},
		{
			// OVH answers application/json with the PNG inside it.
			name:        "an OpenAI images envelope",
			contentType: "application/json",
			payload:     `{"created":1785811350,"data":[{"b64_json":"` + encoded + `"}]}`,
			want:        supportedTrue(),
		},
		{
			// Some providers hand back the whole data URI rather than bare
			// base64. Scoring "not an image" on that would publish a false
			// negative about an endpoint that works.
			name:        "an envelope carrying a data URI",
			contentType: "application/json",
			payload:     `{"data":[{"b64_json":"data:image/png;base64,` + encoded + `"}]}`,
			want:        supportedTrue(),
		},
		{
			name:        "an envelope whose base64 is not an image",
			contentType: "application/json",
			payload:     `{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("not an image at all")) + `"}]}`,
			want:        supportedFalse(),
		},
		{
			// A link proves nothing and fetching it would be a second request
			// for one capability in one cycle.
			name:        "an envelope carrying only a URL",
			contentType: "application/json",
			payload:     `{"data":[{"url":"https://example.test/generated.png"}]}`,
			want:        nil,
		},
		{
			name:        "an empty envelope",
			contentType: "application/json",
			payload:     `{"data":[]}`,
			want:        supportedFalse(),
		},
		{
			name:        "a 200 that is neither bytes nor an envelope",
			contentType: "text/html",
			payload:     "<html>service unavailable</html>",
			want:        supportedFalse(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, detail := verifyImageBytes(testCase.contentType, []byte(testCase.payload))
			wantVerdict(t, got, testCase.want, detail)
		})
	}
}

// Every one of these statuses was seen from a real provider on 2026-08-04. Only
// the deliberate rejections may set a verdict; the rest say nothing about the
// feature and must leave the stored answer alone.
func TestOnlyADeliberateRejectionSetsAVerdict(t *testing.T) {
	cases := []struct {
		name        string
		outcome     string
		status      int
		routeProven bool
		want        *bool
	}{
		{"OVH: feature 'tool calls' is not currently supported", OutcomeClientError, http.StatusBadRequest, true, supportedFalse()},
		{"llm7: upstream rejected the json_schema request", OutcomeClientError, http.StatusMethodNotAllowed, true, supportedFalse()},
		{"unprocessable request body", OutcomeClientError, http.StatusUnprocessableEntity, true, supportedFalse()},
		{"a 404 is about the route, not the model", OutcomeClientError, http.StatusNotFound, true, nil},
		// The image path has never answered anything, so a method refusal there
		// is as likely to be a wrong seeded path or a proxy as a fact about the
		// model.
		{"a method refusal on a path we have never seen work", OutcomeClientError, http.StatusMethodNotAllowed, false, nil},
		{"a rejected image request body", OutcomeClientError, http.StatusBadRequest, false, supportedFalse()},
		{"OVH rate limit", OutcomeRateLimited, http.StatusTooManyRequests, true, nil},
		{"pollinations anonymous pool empty", OutcomeRateLimited, http.StatusPaymentRequired, true, nil},
		{"a provider that started demanding a key", OutcomeNeedsKey, http.StatusUnauthorized, true, nil},
		{"an edge turning us away", OutcomeBlocked, http.StatusForbidden, true, nil},
		{"a bad gateway", OutcomeServerError, http.StatusBadGateway, true, nil},
		{"a timeout", OutcomeTimeout, 0, true, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := refusalVerdict(testCase.outcome, testCase.status, testCase.routeProven)
			wantVerdict(t, got, testCase.want, "")
		})
	}
}

// The rule the whole project turns on, at the point where it is written to the
// database: a probe that settled nothing moves the timestamp and the reason and
// leaves the verdict alone.
func TestAnInconclusiveProbeNeverOverwritesAVerdict(t *testing.T) {
	inconclusive := capabilityUpdates(capabilityResult{})
	if slices.Contains(inconclusive, "supported") {
		t.Fatalf("a settled-nothing probe must not write a verdict: %v", inconclusive)
	}
	if !slices.Contains(inconclusive, "last_checked") || !slices.Contains(inconclusive, "last_error") {
		t.Fatalf("a settled-nothing probe must still record that it happened: %v", inconclusive)
	}
	if slices.Contains(inconclusive, "last_ok") {
		t.Fatalf("nothing worked, so nothing worked at a time: %v", inconclusive)
	}

	failed := capabilityUpdates(capabilityResult{Supported: supportedFalse()})
	if !slices.Contains(failed, "supported") || slices.Contains(failed, "last_ok") {
		t.Fatalf("a clean negative writes the verdict and not the success time: %v", failed)
	}

	worked := capabilityUpdates(capabilityResult{Supported: supportedTrue()})
	if !slices.Contains(worked, "supported") || !slices.Contains(worked, "last_ok") {
		t.Fatalf("a verified yes writes both: %v", worked)
	}
}

func TestCapabilityRequestsCarryWhatTheyClaimTo(t *testing.T) {
	tools, err := capabilityBody("gpt-oss:20b", CapabilityTools)
	if err != nil {
		t.Fatalf("capabilityBody: %v", err)
	}
	if !strings.Contains(string(tools), `"tools":[`) || !strings.Contains(string(tools), "get_weather") {
		t.Fatalf("tools probe carries no tool definition: %s", tools)
	}
	schema, err := capabilityBody("gpt-oss:20b", CapabilityJSONSchema)
	if err != nil {
		t.Fatalf("capabilityBody: %v", err)
	}
	if !strings.Contains(string(schema), `"type":"json_schema"`) {
		t.Fatalf("json_schema probe does not ask for a schema: %s", schema)
	}
	vision, err := capabilityBody("gpt-oss:20b", CapabilityVision)
	if err != nil {
		t.Fatalf("capabilityBody: %v", err)
	}
	if !strings.Contains(string(vision), "data:image/png;base64,") {
		t.Fatalf("vision probe carries no image: %s", vision)
	}
	if _, err := capabilityBody("gpt-oss:20b", "telepathy"); err == nil {
		t.Fatal("expected an error for a capability with no probe defined")
	}
}

// The probe image has to be a real PNG or the vision verdict measures our own
// encoder rather than the provider's model.
func TestVisionProbeImageIsARealPNG(t *testing.T) {
	const prefix = "data:image/png;base64,"
	uri := visionImageDataURI()
	if !strings.HasPrefix(uri, prefix) {
		t.Fatalf("unexpected data URI: %.40s", uri)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, prefix))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !looksLikeImage(decoded) {
		t.Fatal("the vision probe is not sending an image")
	}
}

// The recorded reason now quotes the model's own prose, and models answer in
// their own scripts. A byte-sliced multibyte character is rejected by Postgres
// as an invalid encoding, which fails the insert and takes the cycle with it.
func TestRecordedReasonsAreAlwaysValidUTF8(t *testing.T) {
	long := strings.Repeat("红", 400) // 1200 bytes, cut lands mid-character
	excerpt := firstLine([]byte(long))
	if !utf8.ValidString(excerpt) {
		t.Fatalf("firstLine produced invalid UTF-8: %q", excerpt)
	}
	if excerpt == "" || len([]rune(excerpt)) >= 400 {
		t.Fatalf("firstLine did not clip: %d runes", len([]rune(excerpt)))
	}
	// Bytes that are not text at all reach here too: an image probe records the
	// first line of whatever came back.
	if got := firstLine([]byte{0xFF, 0xD8, 0xFF, 'j', 'p', 'e', 'g'}); !utf8.ValidString(got) {
		t.Fatalf("firstLine passed raw bytes through: %q", got)
	}
	if got := truncate(long, 300); !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
}

func TestImageRequestShapes(t *testing.T) {
	openai := Endpoint{
		Slug:      "ovh-anonymous",
		BaseURL:   "https://oai.endpoints.kepler.ai.cloud.ovh.net",
		ImagePath: "/v1/images/generations",
		ImageMode: ImageModeOpenAI,
	}
	method, target, body, err := imageRequest(openai, "stable-diffusion-xl-base-v10")
	if err != nil {
		t.Fatalf("imageRequest: %v", err)
	}
	if method != http.MethodPost ||
		target != "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1/images/generations" ||
		!strings.Contains(string(body), "stable-diffusion-xl-base-v10") {
		t.Fatalf("got %s %s %s", method, target, body)
	}

	prompt := Endpoint{
		Slug:      "pollinations",
		BaseURL:   "https://text.pollinations.ai",
		ImagePath: "https://image.pollinations.ai/prompt/",
		ImageMode: ImageModePromptURL,
	}
	method, target, body, err = imageRequest(prompt, "sana")
	if err != nil {
		t.Fatalf("imageRequest: %v", err)
	}
	if method != http.MethodGet || body != nil {
		t.Fatalf("prompt URL mode must be a bodiless GET, got %s with %d bytes", method, len(body))
	}
	if !strings.HasPrefix(target, "https://image.pollinations.ai/prompt/") ||
		!strings.Contains(target, "model=sana") {
		t.Fatalf("unexpected target %s", target)
	}

	if _, _, _, err := imageRequest(Endpoint{Slug: "llm7"}, "anything"); err == nil {
		t.Fatal("an endpoint with no image surface must not produce a request")
	}
}
