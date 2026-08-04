package audit

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/bon5co/godjango/database"
	"github.com/bon5co/godjango/management"
)

// defaultChatProbesPerCycle caps how many models one endpoint gets chat-probed
// per cycle when it does not set its own. llm7 alone lists 35; probing all of
// them hourly would be abuse of somebody else's free service.
// Least-recently-checked wins, so coverage still rotates through the whole list
// over a day.
//
// An endpoint whose provider publishes a tighter limit overrides this in its
// seed row -- see Endpoint.ChatProbesPerCycle.
const defaultChatProbesPerCycle = 3

func Commands(services management.ProjectServices) []management.Command {
	return []management.Command{
		{
			Name:    "seedllm",
			Summary: "Insert or refresh the seeded endpoint claims, both shelves",
			Run: func(ctx context.Context, _ []string, streams management.Streams) error {
				return withDatabase(ctx, services, func(db *database.DB) error {
					// The same call the server makes on every cycle, so running
					// this by hand can never produce a different database than
					// letting the deployment do it.
					if err := SeedClaims(ctx, db); err != nil {
						return err
					}
					for _, endpoint := range SeedEndpoints {
						fmt.Fprintf(streams.Out, "seeded %s\n", endpoint.Slug)
					}
					return nil
				})
			},
		},
		{
			Name:    "probellm",
			Summary: "Run one probe cycle over every active endpoint we can reach",
			Run: func(ctx context.Context, _ []string, streams management.Streams) error {
				return withDatabase(ctx, services, func(db *database.DB) error {
					return runCycle(ctx, db, streams.Out)
				})
			},
		},
		{
			Name:    "probecaps",
			Summary: "Run one capability cycle: tools, structured output, vision, image generation",
			Run: func(ctx context.Context, _ []string, streams management.Streams) error {
				return withDatabase(ctx, services, func(db *database.DB) error {
					return runCapabilityCycle(ctx, db, streams.Out, capabilityPause)
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

// runCycle is one polite pass: for each active endpoint we hold what it needs
// to call, one models call, then that endpoint's own budget of chat calls
// against its least recently checked models.
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
		if !prober.CanProbe(endpoint) {
			// A keyed endpoint whose key this deployment does not hold is
			// skipped, and skipped means no row: not a probe, not an outcome,
			// not a gap in a reliability record. Calling it anyway would earn a
			// 401 and publish "this provider is down" when the only thing that
			// is missing is one environment variable on our side.
			fmt.Fprintf(out, "%-14s skipped      no key in %s\n", endpoint.Slug, endpoint.KeyEnv)
			continue
		}
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
			if err := recordChatResult(ctx, db, endpoint, candidate, chat); err != nil {
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
		insert := db.Bun().NewInsert().
			Model(&model).
			On("CONFLICT (endpoint_id, model_id) DO UPDATE").
			Set("last_seen = now()").
			Set("output_modalities = EXCLUDED.output_modalities")
		if !item.FromImageListing {
			// Only the provider's main listing gets to say what a model is for.
			// An id read from an image-only listing that already exists as a
			// chat model would otherwise be flipped to non-chat here, drop out
			// of the shelf, and never be chat-probed again -- silently, with
			// nothing to show that it happened.
			insert = insert.
				Set("tier = EXCLUDED.tier").
				Set("chat_capable = EXCLUDED.chat_capable").
				Set("input_modalities = EXCLUDED.input_modalities")
		}
		if _, err := insert.Returning("id").Exec(ctx); err != nil {
			return fmt.Errorf("record model %s: %w", item.ID, err)
		}
		if err := recordClaims(ctx, db, model.ID, item.Claims); err != nil {
			return err
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
		// proven to refuse us go last, but only for a week -- a provider that
		// opens up a tier would otherwise never be noticed, which is the same
		// staleness this project exists to attack, just pointed inward. Which
		// column holds "refused us" depends on which call this endpoint gets;
		// the name comes from a fixed method on Endpoint, never from input.
		OrderExpr("((m." + endpoint.VerdictColumn() + " IS FALSE)" +
			" AND m.last_checked > now() - interval '7 days') ASC").
		OrderExpr("m.last_checked ASC NULLS FIRST").
		Limit(endpoint.ChatProbesPerCycle()).
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

// recordChatResult writes what the chat probe settled, into the column that
// belongs to the kind of call that was made. A keyed endpoint's success can
// only ever set answered_with_key: writing keyless there would put a model that
// needs a signup on a shelf whose heading promises it does not.
func recordChatResult(
	ctx context.Context,
	db *database.DB,
	endpoint Endpoint,
	model Model,
	probe Probe,
) error {
	if model.ID == 0 {
		return nil // fallback candidate that was never discovered; nothing to update
	}
	now := time.Now()
	verdict := endpoint.VerdictColumn()
	update := db.Bun().NewUpdate().
		Model((*Model)(nil)).
		Set("last_checked = ?", now).
		Where("m.id = ?", model.ID)

	switch probe.Outcome {
	case OutcomeOK:
		update = update.Set(verdict+" = ?", true).Set("last_ok = ?", now)
	case OutcomeNeedsKey:
		// On the keyless shelf this is the provider closing the free door. On
		// the keyed one it is our key being refused for this model, which is
		// usually a model outside the free tier rather than anything wrong.
		update = update.Set(verdict+" = ?", false)
	default:
		// Rate limited, timed out, 5xx: none of these say anything about whether
		// the call is allowed, so the existing verdict stands rather than being
		// overwritten with a guess.
	}
	_, err := update.Exec(ctx)
	return err
}
