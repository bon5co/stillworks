package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/bon5co/godjango/database"
)

// DefaultProbeInterval is how often a deployed instance re-probes. Hourly is
// frequent enough that a provider quietly adding a key requirement is caught the
// same day, and infrequent enough to stay a guest rather than a nuisance: one
// models call plus that endpoint's own small budget of chat calls per cycle,
// which is one for the providers whose published limits are tightest.
const DefaultProbeInterval = time.Hour

// SeedClaims inserts the seeded endpoint claims and refreshes the ones already
// stored. It upserts rather than skipping a non-empty table, which is how the
// key-required shelf came to be empty in production: the three new endpoints
// shipped in the image, the deployment already held four rows, and the old
// "only seed when empty" rule meant nothing was ever inserted. A whole shelf
// was live, keyed, configured -- and blank.
//
// `active` is deliberately not overwritten. An operator who switched an
// endpoint off did so for a reason, and a deploy is not an argument against it.
func SeedClaims(ctx context.Context, db *database.DB) error {
	for _, endpoint := range SeedEndpoints {
		if _, err := db.Bun().NewInsert().
			Model(&endpoint).
			On("CONFLICT (slug) DO UPDATE").
			Set("provider = EXCLUDED.provider").
			Set("base_url = EXCLUDED.base_url").
			Set("chat_path = EXCLUDED.chat_path").
			Set("models_path = EXCLUDED.models_path").
			// auth_mode and key_env travel together: an endpoint moved between
			// the shelves without its key variable would be probed bare and
			// recorded as needing a key.
			Set("auth_mode = EXCLUDED.auth_mode").
			Set("key_env = EXCLUDED.key_env").
			Set("chat_probes_per_cycle = EXCLUDED.chat_probes_per_cycle").
			Set("docs_url = EXCLUDED.docs_url").
			Set("notes = EXCLUDED.notes").
			Set("openai_compatible = EXCLUDED.openai_compatible").
			Set("image_path = EXCLUDED.image_path").
			Set("image_mode = EXCLUDED.image_mode").
			Set("image_models_path = EXCLUDED.image_models_path").
			Set("updated_at = now()").
			Exec(ctx); err != nil {
			return fmt.Errorf("seed %s: %w", endpoint.Slug, err)
		}
	}
	return nil
}

// StartProber runs probe cycles in the background for the lifetime of ctx.
//
// It lives inside the server binary on purpose. The alternative is a cron entry
// or a systemd unit on the host, which means the deployment is no longer one
// self-contained thing that can be moved, and means a probe schedule that
// silently stops when somebody rebuilds the box.
//
// Cycles never overlap: the next tick is skipped rather than queued if the
// previous cycle is still running.
func StartProber(ctx context.Context, db *database.DB, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = DefaultProbeInterval
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}

	go func() {
		// One cycle at startup so a fresh deployment has data immediately
		// instead of an empty shelf for the first hour.
		runOnce(ctx, db, logger)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce(ctx, db, logger)
			}
		}
	}()
}

// StartCapabilityProber runs the feature cycle on its own, much slower clock.
//
// It is a separate ticker rather than "every twenty-fourth liveness cycle" so
// that slowing the liveness probe down with PROBE_INTERVAL cannot silently
// speed the expensive one up, and so a capability cycle that takes ten minutes
// of deliberate pauses never delays a liveness cycle.
//
// The first run is delayed rather than immediate: a fresh deployment has no
// keyless verdicts yet, so a capability cycle at startup would find nothing to
// ask about and would have to wait a day to try again.
func StartCapabilityProber(
	ctx context.Context,
	db *database.DB,
	interval time.Duration,
	logger *slog.Logger,
) {
	if interval <= 0 {
		interval = DefaultCapabilityInterval
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}

	go func() {
		// Long enough for the first liveness cycle to have proven which models
		// answer without a key, which is what decides who gets asked.
		if err := sleepOrStop(ctx, capabilityWarmup); err != nil {
			return
		}
		runCapabilitiesOnce(ctx, db, logger)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runCapabilitiesOnce(ctx, db, logger)
			}
		}
	}()
}

// capabilityWarmup is how long the capability cycle waits after boot for the
// liveness cycle to establish who is keyless.
const capabilityWarmup = 5 * time.Minute

func runOnce(ctx context.Context, db *database.DB, logger *slog.Logger) {
	// A cycle is bounded well under the interval so a hung provider cannot
	// stall the schedule indefinitely.
	cycleCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	started := time.Now()
	if err := SeedClaims(cycleCtx, db); err != nil {
		logger.Error("stillworks seed failed", "error", err)
		return
	}
	if err := runCycle(cycleCtx, db, probeLog{logger}); err != nil {
		logger.Error("stillworks probe cycle failed", "error", err)
		return
	}
	logger.Info("stillworks probe cycle complete", "duration", time.Since(started).Round(time.Second))
}

func runCapabilitiesOnce(ctx context.Context, db *database.DB, logger *slog.Logger) {
	// Generous next to the liveness cycle's twenty minutes: this one pauses
	// fifteen seconds between requests on purpose, and an image generation call
	// can take a provider a minute on its own.
	cycleCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()

	started := time.Now()
	if err := runCapabilityCycle(cycleCtx, db, probeLog{logger}, capabilityPause); err != nil {
		// A cycle spends most of its time deliberately waiting between
		// requests, so shutting the process down mid-pause -- or running out of
		// the cycle's own budget -- is a normal way for it to end. Logging
		// either as a failure would cry wolf on every deploy.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.Error("stillworks capability cycle failed", "error", err)
		}
		return
	}
	logger.Info("stillworks capability cycle complete", "duration", time.Since(started).Round(time.Second))
}

// probeLog adapts the cycle's human-readable output onto the structured logger,
// so a deployed instance reports the same lines the CLI prints.
type probeLog struct{ logger *slog.Logger }

func (p probeLog) Write(payload []byte) (int, error) {
	line := string(payload)
	if trimmed := len(line); trimmed > 0 && line[trimmed-1] == '\n' {
		line = line[:trimmed-1]
	}
	if line != "" {
		p.logger.Info("probe", "line", line)
	}
	return len(payload), nil
}
