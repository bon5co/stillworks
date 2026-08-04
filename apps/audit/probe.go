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
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// UserAgent identifies us to every operator whose free infrastructure we touch,
// and carries somewhere to complain to. Probing anonymously would be rude and
// would also make us impossible to ban politely.
const UserAgent = "stillworks/0.1 (+https://stillworks.supercapybara.com; probes keyless LLM endpoints once per cycle)"

// Budget per single HTTP call. Liveness, not benchmarking.
const requestTimeout = 25 * time.Second

// imageRequestTimeout is the budget for an image generation probe. Drawing a
// picture is not answering a question: OVH's only supported size is 1024x1024
// and the reply is megabytes of base64, so the liveness budget would time out on
// a working endpoint and leave the verdict permanently unverified.
const imageRequestTimeout = 3 * time.Minute

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
	Client *http.Client
	// ImageClient is the same thing with a longer patience, used only by the
	// image generation probe.
	ImageClient *http.Client
	UserAgent   string
	Now         func() time.Time
	// Keys reads our free-tier credential for a keyed endpoint, by environment
	// variable name. It is a field rather than a direct os.Getenv call so a test
	// can exercise the keyed path without putting a secret in the environment of
	// the whole test binary.
	Keys func(name string) string
}

func NewProber() *Prober {
	return &Prober{
		Client:      &http.Client{Timeout: requestTimeout},
		ImageClient: &http.Client{Timeout: imageRequestTimeout},
		UserAgent:   UserAgent,
		Now:         time.Now,
		Keys:        os.Getenv,
	}
}

// KeyFor returns our credential for an endpoint, and whether we hold one.
//
// A keyless endpoint has none and needs none, so it reports false and the
// request goes out bare -- which is the entire claim the first shelf makes.
// A keyed endpoint whose variable is unset also reports false, and the caller's
// job is then to skip it rather than to call it: an unauthenticated request to
// a provider that requires a key would come back 401 and, recorded, would
// publish "this provider demands a key" as though we had discovered something,
// when all we had discovered is that our own deployment is misconfigured.
func (p *Prober) KeyFor(endpoint Endpoint) (string, bool) {
	if !endpoint.RequiresKey() || endpoint.KeyEnv == "" {
		return "", false
	}
	reader := p.Keys
	if reader == nil {
		reader = os.Getenv
	}
	key := strings.TrimSpace(reader(endpoint.KeyEnv))
	if key == "" {
		return "", false
	}
	return key, true
}

// CanProbe reports whether we are in a position to ask this endpoint anything.
// The only way to be told no is a keyed endpoint whose key we do not hold.
func (p *Prober) CanProbe(endpoint Endpoint) bool {
	if !endpoint.RequiresKey() {
		return true
	}
	_, held := p.KeyFor(endpoint)
	return held
}

// authorization is the header value for one endpoint, or empty for none. Every
// request in this file goes through it, so "send the key" and "send nothing"
// are decided in exactly one place from the endpoint's own auth mode rather
// than at each call site.
func (p *Prober) authorization(endpoint Endpoint) string {
	key, held := p.KeyFor(endpoint)
	if !held {
		return ""
	}
	return "Bearer " + key
}

// redactKey removes our credential from anything about to be written down. The
// probe log is public -- the raw table is on every endpoint page, and the whole
// point of it is that a visitor can check our working -- so a provider that
// echoes the offending key back inside a 401 body would otherwise publish it.
// No provider we probe is known to do that; the cost of assuming none ever will
// is one leaked key that can never be un-leaked.
func redactKey(text, key string) string {
	if key == "" || text == "" {
		return text
	}
	return strings.ReplaceAll(text, key, "[redacted]")
}

// scrub is the single exit through which a keyed probe's recorded text passes.
func (p *Prober) scrub(endpoint Endpoint, probe Probe) Probe {
	key, held := p.KeyFor(endpoint)
	if !held {
		return probe
	}
	probe.Error = redactKey(probe.Error, key)
	return probe
}

// DiscoveredModel is a model id an endpoint says it offers.
type DiscoveredModel struct {
	ID          string
	Tier        string
	ChatCapable bool
	InputModes  string
	OutputModes string
	// FromImageListing marks a model read from a provider's separate image
	// listing, which is allowed to add an image modality but never to take a
	// chat verdict away from an id that also appears in the text listing.
	FromImageListing bool
	// Claims is what the provider's own listing says about each capability,
	// keyed by capability name and absent where it says nothing. It is recorded
	// beside the measurement, never instead of it: llm7 published tools:true
	// for every turbo model and vision:false for all of them on 2026-08-04, and
	// both statements needed a real call before either could be repeated.
	Claims map[string]bool
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// responseLimit is how much of a JSON reply is worth reading. An endpoint that
// wants to send more than a megabyte in answer to "ping" is not answering.
const responseLimit = 1 << 20

// do issues one probe request against an endpoint, authenticated exactly as
// that endpoint's auth mode says. The endpoint is passed rather than a header,
// so a call site cannot decide for itself whether to send a credential.
func (p *Prober) do(
	ctx context.Context,
	endpoint Endpoint,
	method, target string,
	body []byte,
) (int, []byte, error) {
	status, _, payload, _, err := p.doRaw(
		ctx, p.Client, p.credentialFor(endpoint, target), method, target, body, responseLimit)
	return status, payload, err
}

// credentialFor is authorization narrowed to one destination. A path column may
// hold an absolute URL -- Pollinations' image listing does, and OpenRouter's
// model listing does because its query string is part of the question being
// asked -- and an absolute URL can name any host at all. Sending our key
// wherever a seed row points would turn one careless edit, or one row written
// straight into the database, into a credential handed to a stranger.
//
// So the key travels only to the host the endpoint's own base URL names. A
// mismatch drops the header rather than the request: the probe still happens,
// it just goes out bare, and whatever answers is recorded honestly.
func (p *Prober) credentialFor(endpoint Endpoint, target string) string {
	if !sameHost(endpoint.BaseURL, target) {
		return ""
	}
	return p.authorization(endpoint)
}

func sameHost(base, target string) bool {
	parsedBase, baseErr := url.Parse(base)
	parsedTarget, targetErr := url.Parse(target)
	if baseErr != nil || targetErr != nil {
		return false
	}
	return parsedBase.Host != "" && strings.EqualFold(parsedBase.Host, parsedTarget.Host)
}

// doUnauthenticated is the request path for everything that is not a probe: the
// visitor-triggered test call. It takes no endpoint at all, so our key cannot
// reach it however the route above it is later rewritten. Handing a visitor a
// call made on our credential would be free inference on our quota, dressed as
// a measurement of what they can reach.
func (p *Prober) doUnauthenticated(
	ctx context.Context,
	method, target string,
	body []byte,
) (int, []byte, error) {
	status, _, payload, _, err := p.doRaw(ctx, p.Client, "", method, target, body, responseLimit)
	return status, payload, err
}

// doRaw is the single request every probe goes through. It reports the response
// headers, because an image probe cannot judge its answer without the content
// type, and reports truncation separately from failure: a reply cut off at the
// read limit proves nothing and must not be scored as a bad body.
func (p *Prober) doRaw(
	ctx context.Context,
	client *http.Client,
	authorization string,
	method, endpoint string,
	body []byte,
	limit int64,
) (status int, header http.Header, payload []byte, truncated bool, err error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, nil, false, err
	}
	request.Header.Set("User-Agent", p.UserAgent)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// The header goes on only when the caller resolved one from the endpoint's
	// auth mode, which for a keyless endpoint it never can. The keyless shelf's
	// entire claim is that these requests carry nothing.
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, nil, false, err
	}
	defer response.Body.Close()
	// One byte past the limit, so overflow is visible rather than silent.
	payload, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return response.StatusCode, response.Header, nil, false, err
	}
	if int64(len(payload)) > limit {
		return response.StatusCode, response.Header, payload[:limit], true, nil
	}
	return response.StatusCode, response.Header, payload, false, nil
}

// classify turns a transport error or status code into an outcome. The split
// between needs_key and everything else is the point of the whole project: a
// provider that quietly started demanding a key is still "up", and every other
// list will keep showing it as free.
//
// The outcome names are the same on both shelves and mean the same thing about
// the reply, but not the same thing about the world. On a keyless endpoint a
// needs_key says the free door closed. On a keyed one it says the key we sent
// was refused for that model -- a model outside our free tier, or a key that
// has been revoked. Which reading applies is a property of the endpoint, so it
// is resolved where the endpoint is known rather than guessed at here.
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
//
// On a keyed endpoint the listing is what the free tier shows *our* account.
// Providers do gate model lists by plan, so this is the honest reading of it
// and the reason the keyed shelf says "our key" everywhere rather than "free".
func (p *Prober) ProbeModels(ctx context.Context, endpoint Endpoint) (Probe, []DiscoveredModel) {
	probe, discovered := p.probeListing(ctx, endpoint, endpoint.ModelsPath, KindModels)
	return p.scrub(endpoint, probe), discovered
}

// ProbeImageModels reads a separate image model listing where the provider
// keeps one. Pollinations does: its text listing carries one text model and no
// image model at all, so without this call the image endpoint could only ever
// be claimed, never attributed to a model and verified.
func (p *Prober) ProbeImageModels(ctx context.Context, endpoint Endpoint) (Probe, []DiscoveredModel) {
	probe, discovered := p.probeListing(ctx, endpoint, endpoint.ImageModelsPath, KindImageModels)
	for index := range discovered {
		// These come from an image-only listing, so the modality is a fact
		// about where we read them rather than a guess about their names.
		discovered[index].ChatCapable = false
		discovered[index].OutputModes = "image"
		if discovered[index].Claims == nil {
			discovered[index].Claims = map[string]bool{}
		}
		discovered[index].Claims[CapabilityImageOut] = true
		// An image listing says a model draws. It does not say the same id
		// cannot also chat, and a provider that publishes one id in both
		// listings must not have its chat verdict erased by the second read.
		discovered[index].FromImageListing = true
	}
	return p.scrub(endpoint, probe), discovered
}

func (p *Prober) probeListing(
	ctx context.Context,
	endpoint Endpoint,
	path, kind string,
) (Probe, []DiscoveredModel) {
	started := p.now()
	status, payload, err := p.do(ctx, endpoint, http.MethodGet, joinURL(endpoint.BaseURL, path), nil)
	probe := Probe{
		EndpointID: endpoint.ID,
		StartedAt:  started,
		Kind:       kind,
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

// parseModels tolerates the four shapes actually served in the wild: the OpenAI
// {"data":[{"id":...}]} envelope, a bare array of objects, Ollama's
// {"models":[{"name":...}]}, and a bare array of plain strings --
// image.pollinations.ai/models answers ["sana"] and nothing else.
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
	var names []string
	if err := json.Unmarshal(payload, &names); err == nil {
		var out []DiscoveredModel
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				continue
			}
			out = append(out, DiscoveredModel{ID: name, ChatCapable: chatCapable(name, nil, nil)})
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, errors.New("no model ids found in response")
}

// modelEntry covers the id field names actually served in the wild: OpenAI uses
// "id", Ollama uses "name"/"model". Pollinations additionally publishes
// modalities, which is the only non-guessing way to know a model can chat, and
// llm7 publishes a whole capabilities block. Every capability field is a
// pointer so "the provider said false" stays distinguishable from "the provider
// said nothing", which is the same tri-state discipline the verdicts use.
type modelEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Model       string   `json:"model"`
	Tier        string   `json:"tier"`
	ModelType   string   `json:"model_type"`
	InputModes  []string `json:"input_modalities"`
	OutputModes []string `json:"output_modalities"`
	// llm7 nests the same information one level down.
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	// OpenRouter nests it two levels down, under architecture. Reading it
	// matters more there than anywhere else: that listing is the largest we
	// take and the only one where guessing from the name has to cover music
	// and speech models alongside chat ones.
	Architecture struct {
		InputModes  []string `json:"input_modalities"`
		OutputModes []string `json:"output_modalities"`
	} `json:"architecture"`
	Tools        *bool `json:"tools"`
	Vision       *bool `json:"vision"`
	JSONMode     *bool `json:"json_mode"`
	Capabilities *struct {
		Tools  *bool `json:"tools"`
		Vision *bool `json:"vision"`
		// Mistral spells the same two things differently and adds the one
		// question the name heuristic keeps getting wrong: whether the model
		// serves chat at all. Its listing carries embeddings, OCR, moderation
		// and Voxtral audio models beside the chat ones, and only
		// completion_chat separates them reliably.
		FunctionCalling *bool `json:"function_calling"`
		CompletionChat  *bool `json:"completion_chat"`
		JSONMode        *bool `json:"json_mode"`
	} `json:"capabilities"`
	// Some providers publish capabilities as a list of names rather than
	// booleans: Groq ships supported_features ["tools","json_mode"], OpenRouter
	// ships supported_parameters with "tools" and "structured_outputs" among
	// the sampling knobs. Only presence is read as a claim -- absence from a
	// list is not the provider saying no, and recording it as one would put a
	// false "claimed: no" beside our own measurement.
	SupportedFeatures   []string `json:"supported_features"`
	SupportedParameters []string `json:"supported_parameters"`
}

func (entry modelEntry) inputModes() []string {
	switch {
	case len(entry.InputModes) > 0:
		return entry.InputModes
	case len(entry.Modalities.Input) > 0:
		return entry.Modalities.Input
	default:
		return entry.Architecture.InputModes
	}
}

func (entry modelEntry) outputModes() []string {
	switch {
	case len(entry.OutputModes) > 0:
		return entry.OutputModes
	case len(entry.Modalities.Output) > 0:
		return entry.Modalities.Output
	default:
		return entry.Architecture.OutputModes
	}
}

// claims reads the provider's own capability statements. json_mode is recorded
// against json_object rather than json_schema because that is what the name
// means: llm7 publishes json_mode:true for a model whose json_schema request
// comes back 405, so treating the two as one claim would make our own record
// wrong before a single call was made.
func (entry modelEntry) claims() map[string]bool {
	claimed := map[string]bool{}
	record := func(capability string, value *bool) {
		if value != nil {
			claimed[capability] = *value
		}
	}
	record(CapabilityTools, entry.Tools)
	record(CapabilityVision, entry.Vision)
	record(CapabilityJSONObject, entry.JSONMode)
	if entry.Capabilities != nil {
		record(CapabilityTools, entry.Capabilities.Tools)
		record(CapabilityTools, entry.Capabilities.FunctionCalling)
		record(CapabilityVision, entry.Capabilities.Vision)
		record(CapabilityJSONObject, entry.Capabilities.JSONMode)
	}
	for capability, present := range listedFeatures(entry) {
		if present {
			claimed[capability] = true
		}
	}
	if entry.ModelType == "image" || containsMode(entry.outputModes(), "image") {
		claimed[CapabilityImageOut] = true
	}
	if len(claimed) == 0 {
		return nil
	}
	return claimed
}

// featureNames maps the names providers use in their capability lists onto the
// capabilities we publish a verdict for. json_mode and structured_outputs are
// deliberately different entries: the first is "will emit some JSON", the
// second is "will hold a schema", and llm7 already proved those come apart.
var featureNames = map[string]string{
	"tools":              CapabilityTools,
	"tool_choice":        CapabilityTools,
	"function_calling":   CapabilityTools,
	"json_mode":          CapabilityJSONObject,
	"response_format":    CapabilityJSONObject,
	"structured_outputs": CapabilityJSONSchema,
	"json_schema":        CapabilityJSONSchema,
	"vision":             CapabilityVision,
}

// listedFeatures reads the array-shaped claims. Only presence is returned:
// these lists mix capabilities with sampling parameters, so what is missing
// from one says nothing at all.
func listedFeatures(entry modelEntry) map[string]bool {
	found := map[string]bool{}
	for _, list := range [][]string{entry.SupportedFeatures, entry.SupportedParameters} {
		for _, name := range list {
			if capability, known := featureNames[strings.ToLower(strings.TrimSpace(name))]; known {
				found[capability] = true
			}
		}
	}
	return found
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
		input, output := item.inputModes(), item.outputModes()
		out = append(out, DiscoveredModel{
			ID:          id,
			Tier:        item.Tier,
			ChatCapable: entryChatCapable(item, id, input, output),
			InputModes:  strings.Join(input, ","),
			OutputModes: strings.Join(output, ","),
			Claims:      item.claims(),
		})
	}
	return out
}

// entryChatCapable prefers the provider's own statement over anything we can
// infer. llm7 labels each model chat, image or video and Mistral publishes a
// completion_chat boolean, both of which settle the question that the name
// heuristic only guesses at.
func entryChatCapable(entry modelEntry, id string, input, output []string) bool {
	if entry.ModelType != "" {
		return entry.ModelType == "chat"
	}
	if entry.Capabilities != nil && entry.Capabilities.CompletionChat != nil {
		return *entry.Capabilities.CompletionChat
	}
	return chatCapable(id, input, output)
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

func containsText(modes []string) bool { return containsMode(modes, "text") }

func containsMode(modes []string, want string) bool {
	for _, mode := range modes {
		if strings.EqualFold(mode, want) {
			return true
		}
	}
	return false
}

// ProbeChat asks one model for a handful of tokens. One request, no retry: a
// 429 is recorded and left alone, because retrying a rate limit is how you stop
// being welcome.
//
// On a keyed endpoint the request carries our key, so a 200 proves the model
// answers *us*. What the caller does with that -- and specifically that it must
// never be written to the keyless column -- is decided by the endpoint's
// VerdictColumn, not here.
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
		return p.scrub(endpoint, probe)
	}
	status, payload, err := p.do(
		ctx, endpoint, http.MethodPost, joinURL(endpoint.BaseURL, endpoint.ChatPath), body)
	probe.HTTPStatus = status
	probe.LatencyMS = int(p.now().Sub(started).Milliseconds())
	probe.Outcome = classify(status, payload, err)
	if err != nil {
		probe.Error = err.Error()
		return p.scrub(endpoint, probe)
	}
	if probe.Outcome != OutcomeOK {
		probe.Error = firstLine(payload)
		return p.scrub(endpoint, probe)
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
		return p.scrub(endpoint, probe)
	}
	probe.CompletionChars = len(strings.TrimSpace(answer.Content))
	return p.scrub(endpoint, probe)
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

// firstLine is the short excerpt every recorded reason is built from. It cuts on
// a rune boundary and drops anything that is not valid UTF-8, because the result
// goes into a Postgres TEXT column: since capability probes started quoting the
// model's own prose back into the record -- and models answer in their own
// scripts -- a byte-sliced multibyte character would be rejected by the server
// as an invalid encoding, failing the insert and taking the whole cycle with it.
func firstLine(payload []byte) string {
	text := strings.TrimSpace(strings.ToValidUTF8(string(payload), ""))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return clipRunes(text, 300)
}

// clipRunes cuts a string to a byte budget without splitting a character.
func clipRunes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
