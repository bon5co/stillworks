package audit

import (
	"context"
	"time"

	"github.com/bon5co/godjango/database"
)

// TrafficTotals is the whole scoreboard in one row. Crawler hits are carried
// alongside rather than folded in: a search engine indexing the shelf is worth
// knowing about and is not an audience.
type TrafficTotals struct {
	APICalls    int        `bun:"api_calls" json:"api_calls"`
	PageViews   int        `bun:"page_views" json:"page_views"`
	Sessions    int        `bun:"sessions" json:"sessions"`
	APIClients  int        `bun:"api_clients" json:"api_clients"`
	CrawlerHits int        `bun:"crawler_hits" json:"crawler_hits"`
	FirstAt     *time.Time `bun:"first_at" json:"first_recorded_at"`
	LastAt      *time.Time `bun:"last_at" json:"last_recorded_at"`
}

// TrafficDay is one UTC day of the record. UTC and not Asia/Tokyo because every
// other timestamp this site publishes is UTC, and two clocks on one page is how
// a number gets misread.
type TrafficDay struct {
	Day       time.Time `bun:"day" json:"day"`
	Sessions  int       `bun:"sessions" json:"sessions"`
	PageViews int       `bun:"page_views" json:"page_views"`
	APICalls  int       `bun:"api_calls" json:"api_calls"`
}

// TrafficReferrer answers the only question distribution work needs answered:
// which channel actually sent somebody.
type TrafficReferrer struct {
	Host     string `bun:"host" json:"host"`
	Hits     int    `bun:"hits" json:"hits"`
	Visitors int    `bun:"visitors" json:"visitors"`
}

// TrafficPath shows what people asked for, which says whether the API or the
// shelf is doing the work.
type TrafficPath struct {
	Kind string `bun:"kind" json:"kind"`
	Path string `bun:"path" json:"path"`
	Hits int    `bun:"hits" json:"hits"`
}

// TrafficReport is everything the stats page and /api/stats publish.
type TrafficReport struct {
	Totals    TrafficTotals     `json:"totals"`
	Days      []TrafficDay      `json:"days"`
	Referrers []TrafficReferrer `json:"referrers"`
	Paths     []TrafficPath     `json:"paths"`
}

// A session is one visitor hash on one UTC day. The hash already carries the
// date, so counting distinct hashes counts daily uniques without a window
// function and without keeping anything that survives the day.
const sessionCounts = `
	count(*) FILTER (WHERE kind = 'api' AND NOT is_crawler)                     AS api_calls,
	count(*) FILTER (WHERE kind = 'page' AND NOT is_crawler)                    AS page_views,
	count(DISTINCT visitor_hash) FILTER (WHERE kind = 'page' AND NOT is_crawler) AS sessions
`

// Traffic reads the record. One call, three queries, because the stats page
// shows all of it or none of it.
func Traffic(ctx context.Context, db *database.DB, days, limit int) (TrafficReport, error) {
	var report TrafficReport

	if err := db.Bun().NewRaw(`
		SELECT `+sessionCounts+`,
		       count(DISTINCT visitor_hash) FILTER (WHERE kind = 'api' AND NOT is_crawler) AS api_clients,
		       count(*) FILTER (WHERE is_crawler) AS crawler_hits,
		       min(occurred_at) AS first_at,
		       max(occurred_at) AS last_at
		FROM traffic_events
	`).Scan(ctx, &report.Totals); err != nil {
		return report, err
	}

	if err := db.Bun().NewRaw(`
		-- AT TIME ZONE 'UTC' and not a bare date_trunc: the bare form buckets in
		-- whatever timezone the database session happens to have, while the
		-- visitor hash rotates at UTC midnight. Left alone, the daily visitor
		-- counts would not line up with the days they are printed against.
		SELECT date_trunc('day', occurred_at AT TIME ZONE 'UTC') AS day, `+sessionCounts+`
		FROM traffic_events
		WHERE occurred_at > now() - make_interval(days => ?)
		GROUP BY 1
		ORDER BY 1 DESC
	`, days).Scan(ctx, &report.Days); err != nil {
		return report, err
	}

	if err := db.Bun().NewRaw(`
		SELECT referrer_host AS host,
		       count(*) AS hits,
		       count(DISTINCT visitor_hash) AS visitors
		FROM traffic_events
		WHERE referrer_host <> '' AND NOT is_crawler
		GROUP BY 1
		ORDER BY hits DESC, host
		LIMIT ?
	`, limit).Scan(ctx, &report.Referrers); err != nil {
		return report, err
	}

	if err := db.Bun().NewRaw(`
		SELECT kind, path, count(*) AS hits
		FROM traffic_events
		WHERE NOT is_crawler
		GROUP BY 1, 2
		ORDER BY hits DESC, path
		LIMIT ?
	`, limit).Scan(ctx, &report.Paths); err != nil {
		return report, err
	}

	return report, nil
}
