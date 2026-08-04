package audit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// capabilityMaxTokens is far bigger than the liveness probe's 32 on purpose. A
// tool call, a schema-shaped object and a colour name all have to fit inside the
// budget or the model gets recorded as unable to do a thing it can do: llm7's
// gpt-oss:20b spent 484 tokens of hidden reasoning before emitting
// {"city":"Paris","country":"France"}, and OVH's Qwen3.6-27B ran past 256 with
// nothing but reasoning to show for it, both on 2026-08-04. Anything cut off at
// the budget is recorded as inconclusive rather than as a failure, so the cost
// of being stingy is a question that never gets answered.
const capabilityMaxTokens = 512

// imageResponseLimit caps what an image probe will read. OVH's only supported
// size returns 2.5 MB of base64 for one 1024x1024 PNG, and the magic bytes that
// settle the question are in the first few of them.
const imageResponseLimit = 6 << 20

// capabilityResult is one capability probe: the append-only evidence row, plus
// what it concluded. Supported is nil when the attempt settled nothing -- a
// timeout, a 429, a truncated answer -- and a nil verdict must leave the stored
// one alone. Overwriting a verified "yes" with a "no" because somebody's free
// tier was busy is the failure mode this whole project exists to attack.
type capabilityResult struct {
	Probe     Probe
	Supported *bool
}

func supportedTrue() *bool  { yes := true; return &yes }
func supportedFalse() *bool { no := false; return &no }

// imageClient falls back to the ordinary one for a Prober built by hand in a
// test, so a missing field is a slower verdict rather than a nil dereference.
func (p *Prober) imageClient() *http.Client {
	if p.ImageClient != nil {
		return p.ImageClient
	}
	return p.Client
}

// ProbeCapability runs exactly one request for one model and one capability.
// There is no retry path here for the same reason ProbeChat has none.
func (p *Prober) ProbeCapability(
	ctx context.Context,
	endpoint Endpoint,
	model Model,
	capability string,
) capabilityResult {
	if capability == CapabilityImageOut {
		return p.probeImageGeneration(ctx, endpoint, model)
	}
	return p.probeChatCapability(ctx, endpoint, model, capability)
}

func (p *Prober) probeChatCapability(
	ctx context.Context,
	endpoint Endpoint,
	model Model,
	capability string,
) capabilityResult {
	started := p.now()
	probe := Probe{
		EndpointID: endpoint.ID,
		StartedAt:  started,
		Kind:       capability,
		ModelUsed:  model.ModelID,
	}
	body, err := capabilityBody(model.ModelID, capability)
	if err != nil {
		probe.Outcome = OutcomeBadBody
		probe.Error = err.Error()
		return capabilityResult{Probe: probe}
	}
	status, payload, err := p.do(ctx, http.MethodPost, joinURL(endpoint.BaseURL, endpoint.ChatPath), body)
	probe.HTTPStatus = status
	probe.LatencyMS = int(p.now().Sub(started).Milliseconds())
	probe.Outcome = classify(status, payload, err)
	if err != nil {
		probe.Error = err.Error()
		return capabilityResult{Probe: probe}
	}
	if probe.Outcome != OutcomeOK {
		probe.Error = firstLine(payload)
		return capabilityResult{Probe: probe, Supported: refusalVerdict(probe.Outcome, status)}
	}
	// A 200 that did not do the thing is a real negative, not an error, so the
	// outcome stays "ok": the endpoint answered, it just answered without a
	// tool call. Recording it as a failed probe would drag the endpoint's
	// reliability record down over a question about a feature.
	verdict, detail := verifyCapability(capability, payload)
	probe.Error = detail
	probe.CompletionChars = len(payload)
	return capabilityResult{Probe: probe, Supported: verdict}
}

// refusalVerdict decides what a non-200 says about a capability. Only a
// deliberate rejection of a well-formed request counts as a verified "no":
// OVH answered 400 "feature 'tool calls' is not currently supported" and llm7
// answered 400 "Model 'gpt-oss:20b' does not support vision input" on
// 2026-08-04, and both are exactly the fact we want to publish. A 401, a 403, a
// 429 or a 5xx says nothing about the feature at all, and a 402 from an
// exhausted anonymous pool -- Pollinations answered all four probes that way on
// 2026-08-04 -- says even less.
func refusalVerdict(outcome string, status int) *bool {
	if outcome != OutcomeClientError {
		return nil
	}
	switch status {
	case http.StatusBadRequest,
		http.StatusMethodNotAllowed,
		http.StatusUnsupportedMediaType,
		http.StatusUnprocessableEntity:
		return supportedFalse()
	default:
		// 404 and friends are about the route, not the model.
		return nil
	}
}

func capabilityBody(model, capability string) ([]byte, error) {
	request := map[string]any{
		"model":      model,
		"max_tokens": capabilityMaxTokens,
		"stream":     false,
	}
	switch capability {
	case CapabilityTools:
		request["messages"] = []map[string]any{{
			"role":    "user",
			"content": "What is the weather in Paris right now? Call the tool.",
		}}
		request["tools"] = []any{weatherTool}
		request["tool_choice"] = "auto"
	case CapabilityJSONSchema:
		request["messages"] = []map[string]any{{
			"role":    "user",
			"content": "Give the capital of France as JSON with keys city and country.",
		}}
		request["response_format"] = capitalSchema
	case CapabilityJSONObject:
		request["messages"] = []map[string]any{{
			"role":    "user",
			"content": "Give the capital of France as JSON with keys city and country.",
		}}
		request["response_format"] = map[string]any{"type": "json_object"}
	case CapabilityVision:
		request["messages"] = []map[string]any{{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "What single colour fills this image? Answer with one word."},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": visionImageDataURI()}},
			},
		}}
	default:
		return nil, fmt.Errorf("no probe defined for capability %q", capability)
	}
	return json.Marshal(request)
}

// weatherTool forces the decision rather than inviting it: a one-argument
// lookup the model cannot answer from its own weights, so a model that can call
// tools has no reason not to.
var weatherTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "get_weather",
		"description": "Look up the current weather for one city.",
		"parameters": map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"city": map[string]any{"type": "string"}},
			"required":             []string{"city"},
			"additionalProperties": false,
		},
	},
}

// capitalSchema asks for two required strings with a known answer, so a reply
// can be checked against the schema instead of merely parsed.
var capitalSchema = map[string]any{
	"type": "json_schema",
	"json_schema": map[string]any{
		"name":   "capital",
		"strict": true,
		"schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city":    map[string]any{"type": "string"},
				"country": map[string]any{"type": "string"},
			},
			"required":             []string{"city", "country"},
			"additionalProperties": false,
		},
	},
}

// visionImageDataURI is an 8x8 solid red PNG, encoded here rather than pasted
// in as a base64 constant: a blob in the source cannot be reviewed, and if it
// were subtly not a PNG the probe would be measuring our own encoder.
var visionImageDataURI = sync.OnceValue(func() string {
	square := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			square.Set(x, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, square); err != nil {
		panic(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
})

func verifyCapability(capability string, payload []byte) (*bool, string) {
	switch capability {
	case CapabilityTools:
		return verifyToolCall(payload)
	case CapabilityJSONSchema:
		return verifyStructuredOutput(payload, true)
	case CapabilityJSONObject:
		return verifyStructuredOutput(payload, false)
	case CapabilityVision:
		return verifyVisionAnswer(payload)
	default:
		return nil, fmt.Sprintf("no verifier for capability %q", capability)
	}
}

// chatResponse is the subset of an OpenAI chat completion a capability verdict
// can be read from. tool_calls and the legacy function_call are both accepted
// because both are still served.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			Reasoning string     `json:"reasoning"`
			ToolCalls []toolCall `json:"tool_calls"`
			// function_call is the pre-2023 single-call shape. Providers
			// wrapping older upstreams still emit it.
			FunctionCall *toolFunction `json:"function_call"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON document inside a JSON string, which is the part
	// providers get wrong: an empty string or a prose sentence here means a
	// caller's tool loop breaks on first use.
	Arguments string `json:"arguments"`
}

// verifyToolCall requires a call the caller could actually dispatch: a function
// name, and arguments that parse as a JSON object. A 200 carrying a friendly
// paragraph about the weather is a verified "no" -- checking the status code
// would have called it a yes.
func verifyToolCall(payload []byte) (*bool, string) {
	var response chatResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, "unparseable completion: " + err.Error()
	}
	if len(response.Choices) == 0 {
		return nil, "200 with no choices"
	}
	choice := response.Choices[0]
	calls := choice.Message.ToolCalls
	if len(calls) == 0 && choice.Message.FunctionCall != nil {
		calls = []toolCall{{Type: "function", Function: *choice.Message.FunctionCall}}
	}
	if len(calls) == 0 {
		if choice.FinishReason == "length" {
			// Cut off mid-answer. The model may have been about to call the
			// tool; the probe was stingy, and a stingy probe is not evidence.
			return nil, "truncated at the token budget before any tool call"
		}
		return supportedFalse(), "answered without calling the tool"
	}
	call := calls[0]
	if strings.TrimSpace(call.Function.Name) == "" {
		return supportedFalse(), "tool call carried no function name"
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
		return supportedFalse(), "tool call arguments are not a JSON object: " + firstLine([]byte(call.Function.Arguments))
	}
	return supportedTrue(), ""
}

// verifyStructuredOutput checks the reply is machine-readable, and when a
// schema was demanded, that it matches the schema. A fenced ```json block is a
// failure on purpose: it means the provider ignored response_format and the
// model merely formatted nicely, which is exactly the thing a caller cannot
// build on.
func verifyStructuredOutput(payload []byte, requireSchema bool) (*bool, string) {
	var response chatResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, "unparseable completion: " + err.Error()
	}
	if len(response.Choices) == 0 {
		return nil, "200 with no choices"
	}
	choice := response.Choices[0]
	content := strings.TrimSpace(choice.Message.Content)
	if content == "" {
		if choice.FinishReason == "length" || choice.Message.Reasoning != "" {
			return nil, "no content: the token budget went on reasoning"
		}
		return supportedFalse(), "200 with empty content"
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(content), &decoded); err != nil {
		if choice.FinishReason == "length" {
			return nil, "content truncated at the token budget before it was valid JSON"
		}
		return supportedFalse(), "content is not a JSON object: " + firstLine([]byte(content))
	}
	if !requireSchema {
		return supportedTrue(), ""
	}
	for _, field := range []string{"city", "country"} {
		value, present := decoded[field]
		if !present {
			return supportedFalse(), "JSON is valid but ignores the schema: no " + field
		}
		if _, isText := value.(string); !isText {
			return supportedFalse(), "JSON is valid but " + field + " is not a string"
		}
	}
	if len(decoded) != 2 {
		// additionalProperties:false was part of the schema. A model that
		// invents extra keys is not honouring it, and a caller unmarshalling
		// into a strict struct finds out at runtime.
		return supportedFalse(), "JSON carries keys the schema forbids"
	}
	return supportedTrue(), ""
}

// refusalMarkers are how a text-only model says it cannot see. They are matched
// only against a short reply: a long answer that happens to contain the phrase
// is describing the image, not refusing it.
var refusalMarkers = []string{
	"cannot see", "can't see", "cannot view", "can't view", "unable to see",
	"unable to view", "cannot process image", "don't see any image",
	"no image", "not able to see", "text-based", "text-only",
	"cannot analyze image", "i do not have the ability to see",
}

// redWords are what a model calls the colour we actually sent. Vocabulary is
// generous on purpose -- crimson and scarlet are correct answers, and so is
// answering in the model's own first language.
var redWords = []string{
	"red", "crimson", "scarlet", "maroon", "vermilion", "ruby", "cherry",
	"rouge", "rojo", "rot", "红", "赤",
}

// otherColourWords are the answers that prove the model did not look. A
// text-only model that guesses is caught here; a model that saw the square is
// not, because the square really is red.
var otherColourWords = []string{
	"blue", "green", "yellow", "black", "white", "purple", "violet",
	"orange", "grey", "gray", "brown", "pink", "cyan", "magenta",
}

// verifyVisionAnswer checks the model described the image we sent rather than
// merely producing prose. Accepting any non-refusal would pass a text-only
// model that cheerfully invents an answer, which is the same mistake as
// accepting a 200 for a tool call.
func verifyVisionAnswer(payload []byte) (*bool, string) {
	var response chatResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, "unparseable completion: " + err.Error()
	}
	if len(response.Choices) == 0 {
		return nil, "200 with no choices"
	}
	choice := response.Choices[0]
	answer := strings.TrimSpace(choice.Message.Content)
	if answer == "" {
		if choice.FinishReason == "length" || choice.Message.Reasoning != "" {
			return nil, "no content: the token budget went on reasoning"
		}
		return supportedFalse(), "200 with empty content"
	}
	lowered := strings.ToLower(answer)
	for _, marker := range refusalMarkers {
		if strings.Contains(lowered, marker) {
			return supportedFalse(), "answered without looking: " + firstLine([]byte(answer))
		}
	}
	if containsAny(lowered, redWords) {
		return supportedTrue(), ""
	}
	if containsAny(lowered, otherColourWords) {
		return supportedFalse(), "named a colour the image is not: " + firstLine([]byte(answer))
	}
	// It answered, it did not refuse, and it named no colour at all. That is
	// our question being poorly understood rather than a fact about the model,
	// so the verdict stands and the answer is kept to be read.
	return nil, "no colour named in: " + firstLine([]byte(answer))
}

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// probeImageGeneration verifies that real image bytes come back, in whichever
// of the two shapes the provider serves. "It returned 200" is not enough and
// neither is the content type on its own: OVH's answer is application/json with
// the PNG hidden inside it as base64.
func (p *Prober) probeImageGeneration(
	ctx context.Context,
	endpoint Endpoint,
	model Model,
) capabilityResult {
	started := p.now()
	probe := Probe{
		EndpointID: endpoint.ID,
		StartedAt:  started,
		Kind:       CapabilityImageOut,
		ModelUsed:  model.ModelID,
	}
	method, target, body, err := imageRequest(endpoint, model.ModelID)
	if err != nil {
		probe.Outcome = OutcomeBadBody
		probe.Error = err.Error()
		return capabilityResult{Probe: probe}
	}
	status, header, payload, truncated, err := p.doRaw(
		ctx, p.imageClient(), method, target, body, imageResponseLimit)
	probe.HTTPStatus = status
	probe.LatencyMS = int(p.now().Sub(started).Milliseconds())
	probe.Outcome = classify(status, payload, err)
	if err != nil {
		probe.Error = err.Error()
		return capabilityResult{Probe: probe}
	}
	if truncated {
		// We stopped reading, so we cannot say what the provider sent. That is
		// our limit, not their failure.
		probe.Outcome = OutcomeBadBody
		probe.Error = "response exceeded the probe's read limit"
		return capabilityResult{Probe: probe}
	}
	if probe.Outcome != OutcomeOK {
		probe.Error = firstLine(payload)
		return capabilityResult{Probe: probe, Supported: refusalVerdict(probe.Outcome, status)}
	}
	verdict, detail := verifyImageBytes(header.Get("Content-Type"), payload)
	probe.Error = detail
	probe.CompletionChars = len(payload)
	return capabilityResult{Probe: probe, Supported: verdict}
}

func imageRequest(endpoint Endpoint, model string) (method, target string, body []byte, err error) {
	const prompt = "a plain red square on a white background"
	switch endpoint.ImageMode {
	case ImageModeOpenAI:
		payload, marshalErr := json.Marshal(map[string]any{
			"model":  model,
			"prompt": prompt,
			"n":      1,
		})
		if marshalErr != nil {
			return "", "", nil, marshalErr
		}
		return http.MethodPost, joinURL(endpoint.BaseURL, endpoint.ImagePath), payload, nil
	case ImageModePromptURL:
		// Smallest size the provider will honour: this probe is asking somebody
		// else's GPU for a picture nobody will look at.
		query := url.Values{
			"width":  {"64"},
			"height": {"64"},
			"nologo": {"true"},
			"model":  {model},
		}
		base := strings.TrimSuffix(joinURL(endpoint.BaseURL, endpoint.ImagePath), "/")
		return http.MethodGet, base + "/" + url.PathEscape(prompt) + "?" + query.Encode(), nil, nil
	default:
		return "", "", nil, fmt.Errorf("endpoint %s publishes no image generation surface", endpoint.Slug)
	}
}

// imageSignatures are the first bytes of the formats a generation endpoint
// actually returns. Checking them is the difference between "an image came
// back" and "a 200 came back".
var imageSignatures = [][]byte{
	{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
	{0xFF, 0xD8, 0xFF},       // JPEG
	{'G', 'I', 'F', '8'},     // GIF
	{'R', 'I', 'F', 'F'},     // WebP container
	{'B', 'M'},               // BMP
	{0x00, 0x00, 0x01, 0x00}, // ICO
}

func looksLikeImage(payload []byte) bool {
	for _, signature := range imageSignatures {
		if bytes.HasPrefix(payload, signature) {
			return true
		}
	}
	return false
}

func verifyImageBytes(contentType string, payload []byte) (*bool, string) {
	if strings.HasPrefix(strings.ToLower(contentType), "image/") {
		if looksLikeImage(payload) {
			return supportedTrue(), ""
		}
		return supportedFalse(), "content type says image, the bytes do not"
	}
	var envelope struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return supportedFalse(), "neither image bytes nor an images envelope: " + firstLine(payload)
	}
	if len(envelope.Data) == 0 {
		return supportedFalse(), "images envelope carried no data"
	}
	entry := envelope.Data[0]
	if entry.B64JSON != "" {
		// Only the head is decoded: the signature settles it, and OVH's answer
		// is 2.5 MB of base64 for one picture.
		head := entry.B64JSON
		const enough = 64
		if len(head) > enough {
			head = head[:enough]
		}
		decoded, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(strings.TrimRight(head, "="))
		if err != nil {
			return supportedFalse(), "b64_json is not base64: " + err.Error()
		}
		if !looksLikeImage(decoded) {
			return supportedFalse(), "b64_json decoded to something that is not an image"
		}
		return supportedTrue(), ""
	}
	if entry.URL != "" {
		// A link is not evidence, and fetching it would be a second request for
		// one capability in one cycle. Left unverified and said so, rather than
		// published as a yes on the strength of a URL.
		return nil, "provider answered with a URL only; bytes not verified in this cycle"
	}
	return supportedFalse(), "images envelope carried neither bytes nor a URL"
}
