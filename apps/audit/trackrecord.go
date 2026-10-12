package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bon5co/godjango/database"
)

// TrackRecord is what the probe table says about one endpoint over the days it
// has been watched. It is the content of a provider page that no other
// directory has: not what the provider claims, but how often a real call
// answered, what the refusals were, and how long a reply took.
//
// Every figure is computed from llm_probes at request time and carries the
// window it was counted over. The page prints the numbers and says what they
// are; it does not decorate them into adjectives.
type TrackRecord struct {
	// Since is the first chat probe on record for the endpoint, so a reader can
	// tell a one-day-old figure from a two-month one.
	Since *time.Time
	// Window is how many days the counts below cover, capped at WindowDays.
	WindowDays int
	Attempts   int
	Answered   int
	// Refusals counts the chat probes in the window that did not answer, by
	// the class of refusal. A 429 and a 402 are different facts for somebody
	// choosing a provider: one is a rate limit, the other a spent budget.
	RateLimited int
	NeedsKey    int
	ServerError int
	Unreachable int
	Other       int
	// MedianMS and P95MS are over the chat probes that answered.
	MedianMS int
	P95MS    int
	// Days is the last fourteen UTC days, oldest first, with a row for a day
	// even when nothing was probed, so a gap in our own measurement is visible
	// as a gap and not smoothed over.
	Days []TrackDay
}

// TrackDay is one UTC day of chat probes.
type TrackDay struct {
	Date     time.Time
	Attempts int
	Answered int
	MedianMS int
}

// TrackWindowDays is the span the headline figures are counted over, and the
// number of rows in the day table.
const TrackWindowDays = 14

// Rate is the answered fraction as a percentage, or -1 when nothing was probed.
func (t TrackRecord) Rate() float64 {
	if t.Attempts == 0 {
		return -1
	}
	return 100 * float64(t.Answered) / float64(t.Attempts)
}

// Refused is every chat probe in the window that did not answer.
func (t TrackRecord) Refused() int { return t.Attempts - t.Answered }

// Sentence is the one-line summary used in the meta description and the lede.
// It states the window, because "99% uptime" with no window is the claim this
// site exists to replace.
func (t TrackRecord) Sentence() string {
	if t.Attempts == 0 {
		return "No chat call has been recorded for this endpoint in the last " +
			fmt.Sprint(TrackWindowDays) + " days."
	}
	return fmt.Sprintf("%d of %d real chat calls answered in %s (%.0f%%), median %d ms.",
		t.Answered, t.Attempts, countOf(t.WindowDays, "day"), t.Rate(), t.MedianMS)
}

// RefusalSentence names what the refusals were, most specific first, or is
// empty when nothing was refused.
func (t TrackRecord) RefusalSentence() string {
	var parts []string
	add := func(count int, label string) {
		if count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, label))
		}
	}
	add(t.RateLimited, "rate-limited (HTTP 429 or the provider's own limit message)")
	add(t.NeedsKey, "refused for a missing or rejected key (401, 402 or 403)")
	add(t.ServerError, "server errors (5xx)")
	add(t.Unreachable, "timeouts or connection failures")
	add(t.Other, "other")
	if len(parts) == 0 {
		return ""
	}
	return "The " + fmt.Sprint(t.Refused()) + " that did not answer were " + strings.Join(parts, ", ") + "."
}

type trackAggregate struct {
	Attempts    int        `bun:"attempts"`
	Answered    int        `bun:"answered"`
	RateLimited int        `bun:"rate_limited"`
	NeedsKey    int        `bun:"needs_key"`
	ServerError int        `bun:"server_error"`
	Unreachable int        `bun:"unreachable"`
	MedianMS    *float64   `bun:"median_ms"`
	P95MS       *float64   `bun:"p95_ms"`
	Since       *time.Time `bun:"since"`
}

type trackDayRow struct {
	Day      time.Time `bun:"day"`
	Attempts int       `bun:"attempts"`
	Answered int       `bun:"answered"`
	MedianMS *float64  `bun:"median_ms"`
}

// EndpointTrackRecord reads the aggregate and the day table for one slug.
func EndpointTrackRecord(ctx context.Context, db *database.DB, slug string) (TrackRecord, error) {
	var aggregate []trackAggregate
	err := db.Bun().NewRaw(`
		SELECT count(*) FILTER (WHERE p.started_at > now() - make_interval(days => ?)) AS attempts,
		       count(*) FILTER (WHERE p.started_at > now() - make_interval(days => ?) AND p.outcome = 'ok') AS answered,
		       count(*) FILTER (WHERE p.started_at > now() - make_interval(days => ?) AND p.outcome = 'rate_limited') AS rate_limited,
		       count(*) FILTER (WHERE p.started_at > now() - make_interval(days => ?) AND p.outcome = 'needs_key') AS needs_key,
		       count(*) FILTER (WHERE p.started_at > now() - make_interval(days => ?) AND p.outcome = 'server_error') AS server_error,
		       count(*) FILTER (WHERE p.started_at > now() - make_interval(days => ?) AND p.outcome IN ('timeout','unreachable')) AS unreachable,
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY p.latency_ms)
		           FILTER (WHERE p.started_at > now() - make_interval(days => ?) AND p.outcome = 'ok') AS median_ms,
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY p.latency_ms)
		           FILTER (WHERE p.started_at > now() - make_interval(days => ?) AND p.outcome = 'ok') AS p95_ms,
		       min(p.started_at) AS since
		FROM llm_probes p
		JOIN llm_endpoints e ON e.id = p.endpoint_id
		WHERE e.slug = ? AND p.kind = 'chat'
	`, TrackWindowDays, TrackWindowDays, TrackWindowDays, TrackWindowDays, TrackWindowDays,
		TrackWindowDays, TrackWindowDays, TrackWindowDays, slug).Scan(ctx, &aggregate)
	if err != nil {
		return TrackRecord{}, err
	}
	var days []trackDayRow
	err = db.Bun().NewRaw(`
		SELECT d::date AS day,
		       count(p.id) AS attempts,
		       count(p.id) FILTER (WHERE p.outcome = 'ok') AS answered,
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY p.latency_ms)
		           FILTER (WHERE p.outcome = 'ok') AS median_ms
		FROM generate_series(
		         ((now() AT TIME ZONE 'UTC')::date - (? - 1))::timestamp,
		         (now() AT TIME ZONE 'UTC')::date::timestamp, interval '1 day') AS d
		LEFT JOIN (
		    SELECT p.id, p.outcome, p.latency_ms, p.started_at
		    FROM llm_probes p JOIN llm_endpoints e ON e.id = p.endpoint_id
		    WHERE e.slug = ? AND p.kind = 'chat'
		) p ON (p.started_at AT TIME ZONE 'UTC')::date = d::date
		GROUP BY d
		ORDER BY d
	`, TrackWindowDays, slug).Scan(ctx, &days)
	if err != nil {
		return TrackRecord{}, err
	}
	return assembleTrackRecord(aggregate, days), nil
}

// assembleTrackRecord is the pure half, so the arithmetic is tested without a
// database.
func assembleTrackRecord(aggregate []trackAggregate, days []trackDayRow) TrackRecord {
	record := TrackRecord{WindowDays: TrackWindowDays}
	if len(aggregate) > 0 {
		a := aggregate[0]
		record.Attempts, record.Answered = a.Attempts, a.Answered
		record.RateLimited, record.NeedsKey = a.RateLimited, a.NeedsKey
		record.ServerError, record.Unreachable = a.ServerError, a.Unreachable
		record.Other = max(0, a.Attempts-a.Answered-a.RateLimited-a.NeedsKey-a.ServerError-a.Unreachable)
		if a.MedianMS != nil {
			record.MedianMS = int(*a.MedianMS + 0.5)
		}
		if a.P95MS != nil {
			record.P95MS = int(*a.P95MS + 0.5)
		}
		record.Since = a.Since
		// A young endpoint has not been watched for the whole window, and the
		// sentence must say so rather than claim fourteen days of record.
		if a.Since != nil {
			if watched := int(time.Since(*a.Since).Hours()/24) + 1; watched < record.WindowDays {
				record.WindowDays = max(1, watched)
			}
		}
	}
	for _, row := range days {
		if record.Since != nil && row.Day.Before(record.Since.UTC().Truncate(24*time.Hour)) {
			continue // before we started watching: not a gap in measurement
		}
		day := TrackDay{Date: row.Day, Attempts: row.Attempts, Answered: row.Answered}
		if row.MedianMS != nil {
			day.MedianMS = int(*row.MedianMS + 0.5)
		}
		record.Days = append(record.Days, day)
	}
	return record
}
