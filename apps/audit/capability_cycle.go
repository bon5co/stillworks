package audit

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/bon5co/godjango/database"
	"github.com/uptrace/bun"
)

// capabilitiesPerCycle caps how many (model, capability) pairs one endpoint
// gets probed per cycle, and the cycle runs daily rather than hourly.
//
// The chat probe asks "are you alive", which changes hourly. A capability probe
// asks "can this model call a tool", which changes when a provider ships a new
// model version -- so the cheap answer is to ask far less often. It is also the
// expensive question to ask: a tool-call probe carries a schema, a vision probe
// carries an image, and an image-generation probe asks somebody else's GPU for
// a picture nobody will look at, which came back as 3.3 MB from OVH on
// 2026-08-04.
//
// Eight pairs a day covers llm7's four verified models in under a week and OVH's
// fifteen in about ten days, then starts again with the oldest. Every published
// verdict carries the date it was measured, so a ten-day-old "yes" is readable
// as a ten-day-old yes rather than as a promise.
const capabilitiesPerCycle = 8

// capabilityPause spaces requests to the same endpoint. OVH's anonymous tier is
// roughly two requests a minute per IP: at fifteen seconds apart it answered 429
// to five of eight capability probes on 2026-08-04, so half the cycle measured
// their rate limiter instead of their models. Thirty seconds costs four minutes
// per endpoint on a cycle that runs once a day and buys back the answers.
const capabilityPause = 30 * time.Second

// DefaultCapabilityInterval is a full day, for the reasons in
// capabilitiesPerCycle above.
const DefaultCapabilityInterval = 24 * time.Hour

// imageOutputMarkers identify a model that draws rather than talks, for
// providers that publish no modalities at all. OVH lists two Stable Diffusion
// builds in the same /v1/models as its chat models with nothing but the name to
// tell them apart.
var imageOutputMarkers = []string{
	"stable-diffusion", "sdxl", "flux", "dall-e", "gpt-image",
	"imagen", "midjourney", "sana", "seedream",
}

// capabilityCandidate is one question to ask of one model this cycle.
type capabilityCandidate struct {
	Model       Model
	Capability  string
	LastChecked *time.Time
}

// runCapabilityCycle is one polite pass over the feature questions. Each pair is
// asked at most once, and the pass is bounded by capabilitiesPerCycle per
// endpoint, so there is no expression in here for "ask again".
func runCapabilityCycle(ctx context.Context, db *database.DB, out io.Writer, pause time.Duration) error {
	var endpoints []Endpoint
	if err := db.Bun().NewSelect().
		Model(&endpoints).
		Where("active").
		// An Ollama-shaped endpoint has no response_format, no tools array and
		// no images route, so there is no capability here to ask it about. Not
		// probing it is the honest outcome: its verdicts stay NULL, which reads
		// as "never verified" rather than as "no".
		Where("openai_compatible OR image_mode <> ''").
		Order("slug").
		Scan(ctx); err != nil {
		return err
	}
	if len(endpoints) == 0 {
		return fmt.Errorf("stillworks: no endpoint publishes a capability surface -- run seedllm first")
	}

	prober := NewProber()
	for _, endpoint := range endpoints {
		if err := discoverImageModels(ctx, db, prober, endpoint, out); err != nil {
			return err
		}
		candidates, err := capabilityCandidates(ctx, db, endpoint)
		if err != nil {
			return err
		}
		for index, candidate := range candidates {
			if index > 0 {
				if err := sleepOrStop(ctx, pause); err != nil {
					return err
				}
			}
			result := prober.ProbeCapability(ctx, endpoint, candidate.Model, candidate.Capability)
			if err := insertProbe(ctx, db, result.Probe); err != nil {
				return err
			}
			if err := recordCapabilityResult(ctx, db, candidate, result); err != nil {
				return err
			}
			fmt.Fprintf(out, "%-14s %-12s %-12s %5dms  %-8s %s\n",
				endpoint.Slug, candidate.Capability, result.Probe.Outcome,
				result.Probe.LatencyMS, verdictWord(result.Supported), candidate.Model.ModelID)
		}
	}
	return nil
}

// discoverImageModels reads the separate image listing, where there is one.
// Without it Pollinations' image endpoint has no model to hang a verdict on.
func discoverImageModels(
	ctx context.Context,
	db *database.DB,
	prober *Prober,
	endpoint Endpoint,
	out io.Writer,
) error {
	if endpoint.ImageModelsPath == "" {
		return nil
	}
	probe, discovered := prober.ProbeImageModels(ctx, endpoint)
	if err := insertProbe(ctx, db, probe); err != nil {
		return err
	}
	fmt.Fprintf(out, "%-14s %-12s %-12s %5dms  %d listed\n",
		endpoint.Slug, KindImageModels, probe.Outcome, probe.LatencyMS, probe.ModelsListed)
	return recordModels(ctx, db, endpoint, discovered)
}

// capabilityCandidates builds this cycle's questions: never-asked first, then
// least recently asked. The pairs are assembled here rather than in SQL because
// which capabilities apply to a model is a judgement -- an image model is not
// asked about tool calls, and a model that has not yet answered a keyless chat
// call is not asked anything at all.
func capabilityCandidates(
	ctx context.Context,
	db *database.DB,
	endpoint Endpoint,
) ([]capabilityCandidate, error) {
	var models []Model
	if err := db.Bun().NewSelect().
		Model(&models).
		Where("endpoint_id = ?", endpoint.ID).
		Scan(ctx); err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, nil
	}
	modelIDs := make([]int64, 0, len(models))
	for _, model := range models {
		modelIDs = append(modelIDs, model.ID)
	}
	var recorded []ModelCapability
	if err := db.Bun().NewSelect().
		Model(&recorded).
		Where("model_id IN (?)", bun.In(modelIDs)).
		Scan(ctx); err != nil {
		return nil, err
	}
	checkedAt := map[string]*time.Time{}
	for _, row := range recorded {
		checkedAt[capabilityKey(row.ModelRowID, row.Capability)] = row.LastChecked
	}

	var candidates []capabilityCandidate
	for _, model := range models {
		for _, capability := range applicableCapabilities(endpoint, model) {
			candidates = append(candidates, capabilityCandidate{
				Model:       model,
				Capability:  capability,
				LastChecked: checkedAt[capabilityKey(model.ID, capability)],
			})
		}
	}
	sort.SliceStable(candidates, func(first, second int) bool {
		left, right := candidates[first].LastChecked, candidates[second].LastChecked
		switch {
		case left == nil && right == nil:
			return candidates[first].Model.ModelID < candidates[second].Model.ModelID
		case left == nil:
			return true
		case right == nil:
			return false
		default:
			return left.Before(*right)
		}
	})
	if len(candidates) > capabilitiesPerCycle {
		candidates = candidates[:capabilitiesPerCycle]
	}
	return candidates, nil
}

func capabilityKey(modelRowID int64, capability string) string {
	return fmt.Sprintf("%d/%s", modelRowID, capability)
}

// applicableCapabilities decides which questions are worth asking of a model.
func applicableCapabilities(endpoint Endpoint, model Model) []string {
	if endpoint.ImageMode != "" && producesImages(model) {
		return []string{CapabilityImageOut}
	}
	if !endpoint.OpenAICompatible || !model.ChatCapable {
		return nil
	}
	// A model that has not answered a keyless chat call is not asked about its
	// features: the answer would be a 401 for every capability, and recording
	// four of those a day against somebody's paid tier is just noise on their
	// logs and ours.
	if model.Keyless == nil || !*model.Keyless {
		return nil
	}
	return ChatCapabilities
}

func producesImages(model Model) bool {
	if containsMode(strings.Split(model.OutputModes, ","), "image") {
		return true
	}
	if model.ChatCapable {
		return false
	}
	lowered := strings.ToLower(model.ModelID)
	for _, marker := range imageOutputMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// recordCapabilityResult writes the verdict and its evidence. A nil verdict
// leaves the stored one exactly as it was: only last_checked and the reason
// move, so the shelf can show "still says yes, last confirmed six days ago,
// today's attempt was rate limited" instead of silently flipping to no.
func recordCapabilityResult(
	ctx context.Context,
	db *database.DB,
	candidate capabilityCandidate,
	result capabilityResult,
) error {
	now := time.Now()
	row := ModelCapability{
		ModelRowID:  candidate.Model.ID,
		Capability:  candidate.Capability,
		Supported:   result.Supported,
		LastChecked: &now,
		LastError:   result.Probe.Error,
	}
	if result.Supported != nil && *result.Supported {
		row.LastOK = &now
	}
	insert := db.Bun().NewInsert().
		Model(&row).
		On("CONFLICT (model_id, capability) DO UPDATE").
		Set("last_checked = EXCLUDED.last_checked").
		Set("last_error = EXCLUDED.last_error")
	if result.Supported != nil {
		insert = insert.Set("supported = EXCLUDED.supported")
	}
	if row.LastOK != nil {
		insert = insert.Set("last_ok = EXCLUDED.last_ok")
	}
	if _, err := insert.Exec(ctx); err != nil {
		return fmt.Errorf("record capability %s for model %d: %w",
			candidate.Capability, candidate.Model.ID, err)
	}
	return recordKeylessSideEffect(ctx, db, candidate.Model, result.Probe)
}

// recordKeylessSideEffect uses what a capability probe incidentally proved. A
// request that carried no Authorization header and got an answer is evidence of
// keylessness whatever it was asking about, and for an image model it is the
// only evidence there will ever be: image models are never chat-probed, so
// without this their keyless verdict would stay NULL forever while their
// image_out verdict said yes.
//
// last_checked is deliberately not touched. It orders the hourly chat rotation,
// and moving it here would let the daily cycle starve models of liveness checks.
func recordKeylessSideEffect(ctx context.Context, db *database.DB, model Model, probe Probe) error {
	update := db.Bun().NewUpdate().Model((*Model)(nil)).Where("m.id = ?", model.ID)
	now := time.Now()
	switch probe.Outcome {
	case OutcomeOK:
		update = update.Set("keyless = ?", true).Set("last_ok = ?", now)
	case OutcomeNeedsKey:
		update = update.Set("keyless = ?", false)
	default:
		return nil
	}
	_, err := update.Exec(ctx)
	return err
}

func verdictWord(supported *bool) string {
	switch {
	case supported == nil:
		return "unchanged"
	case *supported:
		return "yes"
	default:
		return "no"
	}
}

// sleepOrStop waits between requests without outliving a cancelled cycle.
func sleepOrStop(ctx context.Context, pause time.Duration) error {
	if pause <= 0 {
		return nil
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// recordClaims stores what the provider says about a model's features. The
// verdict columns are never written here: a claim is not a measurement, and the
// whole point of this shelf is that the two are shown separately.
func recordClaims(ctx context.Context, db *database.DB, modelRowID int64, claims map[string]bool) error {
	if modelRowID == 0 || len(claims) == 0 {
		return nil
	}
	for _, capability := range Capabilities {
		claimed, present := claims[capability]
		if !present {
			continue
		}
		row := ModelCapability{
			ModelRowID: modelRowID,
			Capability: capability,
			Claimed:    &claimed,
		}
		if _, err := db.Bun().NewInsert().
			Model(&row).
			On("CONFLICT (model_id, capability) DO UPDATE").
			Set("claimed = EXCLUDED.claimed").
			Exec(ctx); err != nil {
			return fmt.Errorf("record claim %s for model %d: %w", capability, modelRowID, err)
		}
	}
	return nil
}
