package audit

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/bon5co/godjango/database"
	"github.com/bon5co/godjango/management"
)

// modelsPerCycle caps how many models one endpoint gets chat-probed per cycle.
// llm7 alone lists 35; probing all of them hourly would be abuse of somebody
// else's free service. Least-recently-checked wins, so coverage still rotates
// through the whole list over a day.
const modelsPerCycle = 3

func Commands(services management.ProjectServices) []management.Command {
	return []management.Command{
		{
			Name:    "seedllm",
			Summary: "Insert or refresh the seeded keyless endpoint claims",
			Run: func(ctx context.Context, _ []string, streams management.Streams) error {
				return withDatabase(ctx, services, func(db *database.DB) error {
					for _, endpoint := range SeedEndpoints {
						_, err := db.Bun().NewInsert().
							Model(&endpoint).
							On("CONFLICT (slug) DO UPDATE").
							Set("provider = EXCLUDED.provider").
							Set("base_url = EXCLUDED.base_url").
							Set("chat_path = EXCLUDED.chat_path").
							Set("models_path = EXCLUDED.models_path").
							Set("docs_url = EXCLUDED.docs_url").
							Set("notes = EXCLUDED.notes").
							Set("openai_compatible = EXCLUDED.openai_compatible").
							Set("updated_at = now()").
							Exec(ctx)
						if err != nil {
							return fmt.Errorf("seed %s: %w", endpoint.Slug, err)
						}
						fmt.Fprintf(streams.Out, "seeded %s\n", endpoint.Slug)
					}
					return nil
				})
			},
		},
		{
			Name:    "probellm",
			Summary: "Run one probe cycle over every active keyless endpoint",
			Run: func(ctx context.Context, _ []string, streams management.Streams) error {
				return withDatabase(ctx, services, func(db *database.DB) error {
					return runCycle(ctx, db, streams.Out)
				})
			},
		},
	}
}

func withDatabase(
	ctx context.Context,
	services management.ProjectServices,
	body func(*database.DB) error,
) error {
	if services.Database == nil {
		return fmt.Errorf("stillworks: project services provide no database opener")
	}
	db, closeDatabase, err := services.Database(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = closeDatabase() }()
	return body(db)
}

// runCycle is one polite pass: for each active endpoint, one models call, then
// at most modelsPerCycle chat calls against the least recently checked models.
func runCycle(ctx context.Context, db *database.DB, out io.Writer) error {
	var endpoints []Endpoint
	if err := db.Bun().NewSelect().
		Model(&endpoints).
		Where("active").
		Order("slug").
		Scan(ctx); err != nil {
		return err
	}
	if len(endpoints) == 0 {
		return fmt.Errorf("stillworks: no active endpoints -- run seedllm first")
	}

	prober := NewProber()
	for _, endpoint := range endpoints {
		probe, discovered := prober.ProbeModels(ctx, endpoint)
		if err := insertProbe(ctx, db, probe); err != nil {
			return err
		}
		fmt.Fprintf(out, "%-14s models  %-12s %5dms  %d listed\n",
			endpoint.Slug, probe.Outcome, probe.LatencyMS, probe.ModelsListed)

		if err := recordModels(ctx, db, endpoint, discovered); err != nil {
			return err
		}

		candidates, err := chatCandidates(ctx, db, endpoint)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			chat := prober.ProbeChat(ctx, endpoint, candidate.ModelID)
			if err := insertProbe(ctx, db, chat); err != nil {
				return err
			}
			if err := recordChatResult(ctx, db, candidate, chat); err != nil {
				return err
			}
			fmt.Fprintf(out, "%-14s chat    %-12s %5dms  %s\n",
				endpoint.Slug, chat.Outcome, chat.LatencyMS, candidate.ModelID)
		}
	}
	return nil
}

func insertProbe(ctx context.Context, db *database.DB, probe Probe) error {
	_, err := db.Bun().NewInsert().Model(&probe).Exec(ctx)
	return err
}

// recordModels upserts what the endpoint currently advertises. last_seen is the
// field that makes a disappearance visible: a model that stops being listed
// stops being touched here, and its staleness becomes a fact we can show
// instead of a silence.
func recordModels(
	ctx context.Context,
	db *database.DB,
	endpoint Endpoint,
	discovered []DiscoveredModel,
) error {
	for _, item := range discovered {
		model := Model{
			EndpointID:  endpoint.ID,
			ModelID:     item.ID,
			Tier:        item.Tier,
			ChatCapable: item.ChatCapable,
			InputModes:  item.InputModes,
			OutputModes: item.OutputModes,
		}
		_, err := db.Bun().NewInsert().
			Model(&model).
			On("CONFLICT (endpoint_id, model_id) DO UPDATE").
			Set("last_seen = now()").
			Set("tier = EXCLUDED.tier").
			Set("chat_capable = EXCLUDED.chat_capable").
			Set("input_modalities = EXCLUDED.input_modalities").
			Set("output_modalities = EXCLUDED.output_modalities").
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("record model %s: %w", item.ID, err)
		}
	}
	return nil
}

// chatCandidates picks which models get a chat probe this cycle: never checked
// first, then least recently checked.
func chatCandidates(ctx context.Context, db *database.DB, endpoint Endpoint) ([]Model, error) {
	var models []Model
	err := db.Bun().NewSelect().
		Model(&models).
		Where("endpoint_id = ?", endpoint.ID).
		Where("chat_capable").
		// Never-checked first, then least recently checked. Models already
		// proven to demand a key go last, but only for a week -- a provider
		// that opens up a tier would otherwise never be noticed, which is the
		// same staleness this project exists to attack, just pointed inward.
		OrderExpr("((m.keyless IS FALSE) AND m.last_checked > now() - interval '7 days') ASC").
		OrderExpr("m.last_checked ASC NULLS FIRST").
		Limit(modelsPerCycle).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	if len(models) > 0 {
		return models, nil
	}
	// Nothing discovered -- the listing call may be down or the endpoint may not
	// publish one. Fall back to the seeded default so it still gets one honest
	// liveness check rather than silently reporting nothing.
	if endpoint.DefaultModel == "" {
		return nil, nil
	}
	return []Model{{EndpointID: endpoint.ID, ModelID: endpoint.DefaultModel}}, nil
}

func recordChatResult(ctx context.Context, db *database.DB, model Model, probe Probe) error {
	if model.ID == 0 {
		return nil // fallback candidate that was never discovered; nothing to update
	}
	now := time.Now()
	update := db.Bun().NewUpdate().
		Model((*Model)(nil)).
		Set("last_checked = ?", now).
		Where("m.id = ?", model.ID)

	switch probe.Outcome {
	case OutcomeOK:
		update = update.Set("keyless = ?", true).Set("last_ok = ?", now)
	case OutcomeNeedsKey:
		update = update.Set("keyless = ?", false)
	default:
		// Rate limited, timed out, 5xx: none of these say anything about whether
		// a key is required, so the existing verdict stands rather than being
		// overwritten with a guess.
	}
	_, err := update.Exec(ctx)
	return err
}
