package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// UserAgent identifies us to every operator whose free infrastructure we touch,
// and carries somewhere to complain to. Probing anonymously would be rude and
// would also make us impossible to ban politely.
const UserAgent = "stillworks/0.1 (+https://stillworks.supercapybara.com; probes keyless LLM endpoints once per cycle)"

// Budget per single HTTP call. Liveness, not benchmarking.
const requestTimeout = 25 * time.Second

// maxCompletionTokens keeps the chat probe as small as is honest. It is not 8:
// reasoning models spend the whole budget on a hidden reasoning field and return
// content:"" with finish_reason:"length", which reads as a broken endpoint when
// the endpoint was fine and the probe was stingy. Measured against llm7's
// gpt-oss:20b, which failed this way on 2026-08-03.
const maxCompletionTokens = 32

// Prober performs one cycle. Its rules are structural rather than advisory:
// there is no retry path in this type, and each method issues exactly one
// request, so "probe an endpoint twice in a cycle" is not expressible.
type Prober struct {
	Client    *http.Client
	UserAgent string
	Now       func() time.Time
}

func NewProber() *Prober {
	return &Prober{
		Client:    &http.Client{Timeout: requestTimeout},
		UserAgent: UserAgent,
		Now:       time.Now,
	}
}

// DiscoveredModel is a model id an endpoint says it offers.
type DiscoveredModel struct {
	ID          string
	Tier        string
	ChatCapable bool
	InputModes  string
	OutputModes string
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prober) do(ctx context.Context, method, endpoint string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("User-Agent", p.UserAgent)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// Deliberately no Authorization header, ever. The entire claim under test is
	// that this works without one.
	response, err := p.Client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, payload, nil
}

// classify turns a transport error or status code into an outcome. The split
// between needs_key and everything else is the point of the whole project: a
// provider that quietly started demanding a key is still "up", and every other
// list will keep showing it as free.
func classify(status int, body []byte, err error) string {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return OutcomeTimeout
		}
		return OutcomeUnreachable
	}
	switch {
	case status == http.StatusUnauthorized:
		return OutcomeNeedsKey
	case status == http.StatusPaymentRequired:
		// 402 is ambiguous in the same way 403 is. Pollinations returned
		// "API key budget too low" to a request carrying no key at all on
		// 2026-08-03, and served 200 again a minute later: the anonymous pool
		// had momentarily run dry. Recording that as needs_key would flip a
		// genuinely keyless provider to "requires a key" on a hiccup.
		if mentionsExhaustedBudget(body) {
			return OutcomeRateLimited
		}
		return OutcomeNeedsKey
	case status == http.StatusForbidden:
		// A 403 is ambiguous. mlvoca.com serves a bare nginx "403 Forbidden"
		// HTML page to every request: the path is closed, nobody is asking for
		// a key. Only call it needs_key when the body actually says so.
		if mentionsCredentials(body) {
			return OutcomeNeedsKey
		}
		return OutcomeBlocked
	case status == http.StatusTooManyRequests:
		return OutcomeRateLimited
	case status >= 500:
		return OutcomeServerError
	case status >= 400:
		return OutcomeClientError
	case status >= 200 && status < 300:
		return OutcomeOK
	default:
		return OutcomeClientError
	}
}

// credentialWords are what an API says when it wants a key. A proxy that simply
// refuses us says none of them.
var credentialWords = []string{
	"api key", "api_key", "apikey", "missing_api_key",
	"unauthorized", "authentication", "authorization",
	"token", "credential", "sign up", "signup", "register",
}

// budgetWords say "you have run out", not "who are you".
var budgetWords = []string{
	"budget", "quota", "credit", "insufficient", "balance",
	"exceeded", "out of funds", "too low",
}

func mentionsExhaustedBudget(body []byte) bool {
	lowered := strings.ToLower(string(body))
	for _, word := range budgetWords {
		if strings.Contains(lowered, word) {
			return true
		}
	}
	return false
}

func mentionsCredentials(body []byte) bool {
	lowered := strings.ToLower(string(body))
	// An HTML error page from an edge proxy is not an API speaking.
	if strings.Contains(lowered, "<html") && !strings.Contains(lowered, "{") {
		return false
	}
	for _, word := range credentialWords {
		if strings.Contains(lowered, word) {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

func joinURL(base, path string) string {
	if path == "" {
		return base
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/" + strings.TrimPrefix(path, "/")
	return parsed.String()
}

// ProbeModels asks an endpoint what it offers. One request, no retry.
func (p *Prober) ProbeModels(ctx context.Context, endpoint Endpoint) (Probe, []DiscoveredModel) {
	started := p.now()
	status, payload, err := p.do(ctx, http.MethodGet, joinURL(endpoint.BaseURL, endpoint.ModelsPath), nil)
	probe := Probe{
		EndpointID: endpoint.ID,
		StartedAt:  started,
		Kind:       KindModels,
		HTTPStatus: status,
		LatencyMS:  int(p.now().Sub(started).Milliseconds()),
		Outcome:    classify(status, payload, err),
	}
	if err != nil {
		probe.Error = err.Error()
		return probe, nil
	}
	if probe.Outcome != OutcomeOK {
		probe.Error = firstLine(payload)
		return probe, nil
	}
	models, parseErr := parseModels(payload)
	if parseErr != nil {
		probe.Outcome = OutcomeBadBody
		probe.Error = parseErr.Error()
		return probe, nil
	}
	probe.ModelsListed = len(models)
	return probe, models
}

// parseModels tolerates the three shapes actually served in the wild: the
// OpenAI {"data":[{"id":...}]} envelope, a bare array, and Ollama's
// {"models":[{"name":...}]}.
func parseModels(payload []byte) ([]DiscoveredModel, error) {
	var envelope struct {
		Data   []modelEntry `json:"data"`
		Models []modelEntry `json:"models"`
	}
	if err := json.Unmarshal(payload, &envelope); err == nil {
		if found := collect(envelope.Data); len(found) > 0 {
			return found, nil
		}
		if found := collect(envelope.Models); len(found) > 0 {
			return found, nil
		}
	}
	var bare []modelEntry
	if err := json.Unmarshal(payload, &bare); err == nil {
		if found := collect(bare); len(found) > 0 {
			return found, nil
		}
	}
	return nil, errors.New("no model ids found in response")
}

// modelEntry covers the id field names actually served in the wild: OpenAI uses
// "id", Ollama uses "name"/"model". Pollinations additionally publishes
// modalities, which is the only non-guessing way to know a model can chat.
type modelEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Model       string   `json:"model"`
	Tier        string   `json:"tier"`
	InputModes  []string `json:"input_modalities"`
	OutputModes []string `json:"output_modalities"`
}

func collect(entries []modelEntry) []DiscoveredModel {
	var out []DiscoveredModel
	for _, item := range entries {
		id := item.ID
		if id == "" {
			id = item.Model
		}
		if id == "" {
			id = item.Name
		}
		if id == "" {
			continue
		}
		out = append(out, DiscoveredModel{
			ID:          id,
			Tier:        item.Tier,
			ChatCapable: chatCapable(id, item.InputModes, item.OutputModes),
			InputModes:  strings.Join(item.InputModes, ","),
			OutputModes: strings.Join(item.OutputModes, ","),
		})
	}
	return out
}

// nonChatMarkers are substrings that identify a model which cannot answer a
// chat completion. Used only when a provider publishes no modality data: OVH
// lists stable-diffusion, bge embeddings and whisper in the same /v1/models as
// its chat models, with nothing to distinguish them but the name.
var nonChatMarkers = []string{
	"stable-diffusion", "sdxl", "flux", "dall-e", "midjourney", "image",
	"embedding", "bge-", "gte-", "e5-", "rerank",
	"whisper", "tts", "speech", "voice", "audio",
	"moderation", "clip", "ocr", "upscal",
}

// chatCapable prefers published modalities and falls back to the name. It is a
// heuristic and is recorded as such: input_modalities/output_modalities are
// stored verbatim so a wrong call here can be found and corrected rather than
// quietly shaping the data forever.
func chatCapable(id string, input, output []string) bool {
	if len(input) > 0 || len(output) > 0 {
		return containsText(input) && containsText(output)
	}
	lowered := strings.ToLower(id)
	for _, marker := range nonChatMarkers {
		if strings.Contains(lowered, marker) {
			return false
		}
	}
	return true
}

func containsText(modes []string) bool {
	for _, mode := range modes {
		if strings.EqualFold(mode, "text") {
			return true
		}
	}
	return false
}

// ProbeChat asks one model for a handful of tokens. One request, no retry: a
// 429 is recorded and left alone, because retrying a rate limit is how you stop
// being welcome.
func (p *Prober) ProbeChat(ctx context.Context, endpoint Endpoint, model string) Probe {
	started := p.now()
	body, err := chatBody(endpoint, model)
	probe := Probe{
		EndpointID: endpoint.ID,
		StartedAt:  started,
		Kind:       KindChat,
		ModelUsed:  model,
	}
	if err != nil {
		probe.Outcome = OutcomeBadBody
		probe.Error = err.Error()
		return probe
	}
	status, payload, err := p.do(ctx, http.MethodPost, joinURL(endpoint.BaseURL, endpoint.ChatPath), body)
	probe.HTTPStatus = status
	probe.LatencyMS = int(p.now().Sub(started).Milliseconds())
	probe.Outcome = classify(status, payload, err)
	if err != nil {
		probe.Error = err.Error()
		return probe
	}
	if probe.Outcome != OutcomeOK {
		probe.Error = firstLine(payload)
		return probe
	}
	answer, parseErr := parseCompletion(payload)
	if parseErr != nil || !answer.Served {
		// A 200 carrying no completion at all is a failure, not a success.
		// Status-only checks sail straight past this and report a working
		// endpoint.
		probe.Outcome = OutcomeBadBody
		if parseErr != nil {
			probe.Error = parseErr.Error()
		} else {
			probe.Error = "200 with no completion in the body"
		}
		return probe
	}
	probe.CompletionChars = len(strings.TrimSpace(answer.Content))
	return probe
}

func chatBody(endpoint Endpoint, model string) ([]byte, error) {
	if endpoint.OpenAICompatible {
		return json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
			"max_tokens": maxCompletionTokens,
			"stream":     false,
		})
	}
	// Ollama-shaped fallback (mlvoca and friends).
	return json.Marshal(map[string]any{
		"model":  model,
		"prompt": "ping",
		"stream": false,
	})
}

// completion is what a chat probe managed to extract. Served reports whether the
// endpoint actually produced tokens, which is not the same as content being
// non-empty: a reasoning model can emit only hidden reasoning and stop on
// length. That is a working endpoint and must not be recorded as a broken one.
type completion struct {
	Content string
	Served  bool
}

func parseCompletion(payload []byte) (completion, error) {
	var body struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning"`
			} `json:"message"`
			Text         string `json:"text"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Response string `json:"response"`
		Usage    struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return completion{}, fmt.Errorf("unparseable completion: %w", err)
	}
	if body.Response != "" {
		return completion{Content: body.Response, Served: true}, nil
	}
	for _, choice := range body.Choices {
		switch {
		case choice.Message.Content != "":
			return completion{Content: choice.Message.Content, Served: true}, nil
		case choice.Text != "":
			return completion{Content: choice.Text, Served: true}, nil
		case choice.Message.Reasoning != "":
			// Reasoning-only answer: the model spoke, we just did not ask for
			// enough tokens to hear the conclusion.
			return completion{Content: choice.Message.Reasoning, Served: true}, nil
		case choice.FinishReason == "length" && body.Usage.CompletionTokens > 0:
			return completion{Served: true}, nil
		}
	}
	return completion{}, nil
}

func firstLine(payload []byte) string {
	text := strings.TrimSpace(string(payload))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	const limit = 300
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}
