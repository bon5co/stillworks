package audit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bon5co/godjango/database"
	"github.com/uptrace/bun"
)

// The sponsored slot.
//
// One paid placement, shown in two places: a clearly labelled card on the shelf,
// and a separate top-level "sponsored" field in /api/llm/up. The second is the
// one that matters, because most of this site's readers are programs.
//
// The rule everything here is built around: a sponsor is never a row on the
// shelf. Callers of /api/llm/up take models[0] as the pick and the rest as the
// fallback chain. If a paid entry could appear in that array, or move anything
// in it, the product -- an honest answer to "what is answering right now" --
// would be for sale. So the sponsor is a different field, a different Go type
// and a different table, and the tests assert the models array is byte-for-byte
// what it would be with no sponsor at all.

const (
	// sponsorClickPath is the first-party redirect every sponsor link goes
	// through, so a click is counted in the same pipeline as everything else.
	sponsorClickPath = "/go/sponsor"
	// sponsorPagePath is the page that sells the slot, and where the house ad
	// points.
	sponsorPagePath = "/sponsor"

	// The two places a click can come from. Recorded in the event path so the
	// report can tell a person following the card from an agent following the
	// link in the API response, which is the one split the traffic table can
	// make honestly.
	clickSourcePage = "page"
	clickSourceAPI  = "api"

	// KindSponsor is the traffic event kind for a click on the sponsor link.
	KindSponsor = "sponsor"

	// sponsorLabel is the word on the card and in the API. It is the whole
	// disclosure; nothing about the slot may be able to render without it.
	sponsorLabel = "Sponsored"
)

// sponsorNotice travels inside every API entry. A program reading the field
// should be told in words what it is looking at, not left to infer from the
// field name.
const sponsorNotice = "Paid placement. Not probed, not ranked, and not part of the models array: " +
	"models[0] is still the pick and models[1:] is still the fallback chain, exactly as ranked by " +
	"our own probes. The probe field is real probe data for the sponsor's endpoint if it is on our " +
	"shelf, and null if it is not."

// Sponsor is one paid placement. Its dates are inclusive UTC dates and the
// database refuses two whose dates overlap, so at most one is ever active.
type Sponsor struct {
	bun.BaseModel `bun:"table:sponsors,alias:sp"`

	ID           int64  `bun:"id,pk,autoincrement"`
	Provider     string `bun:"provider,notnull"`
	Pitch        string `bun:"pitch,notnull"`
	URL          string `bun:"url,notnull"`
	Model        string `bun:"model,notnull"`
	BaseURL      string `bun:"base_url,notnull"`
	EndpointSlug string `bun:"endpoint_slug,notnull"`
	// SubscriptionID is the Polar subscription that paid for this slot, empty for a
	// sponsor set by hand. It is how a renewal or a cancellation finds its row.
	SubscriptionID string    `bun:"subscription_id,nullzero"`
	StartsOn       time.Time `bun:"starts_on,notnull,type:date"`
	EndsOn         time.Time `bun:"ends_on,notnull,type:date"`
	CreatedAt      time.Time `bun:"created_at,nullzero,notnull,default:now()"`
}

// Field limits, mirrored in the migration's CHECK constraints. They are
// checked here first so the person (or agent) setting a sponsor gets a sentence
// rather than a constraint name.
const (
	sponsorProviderMax = 80
	sponsorPitchMax    = 200
)

// Validate refuses a sponsor that could not be shown honestly.
func (s Sponsor) Validate() error {
	provider := strings.TrimSpace(s.Provider)
	switch {
	case provider == "":
		return errors.New("sponsor: provider name is required")
	case len([]rune(provider)) > sponsorProviderMax:
		return fmt.Errorf("sponsor: provider name is over %d characters", sponsorProviderMax)
	}
	pitch := strings.TrimSpace(s.Pitch)
	switch {
	case pitch == "":
		return errors.New("sponsor: pitch is required")
	case len([]rune(pitch)) > sponsorPitchMax:
		return fmt.Errorf("sponsor: pitch is over %d characters -- it has to fit on one card", sponsorPitchMax)
	}
	if err := validHTTPS(s.URL); err != nil {
		return fmt.Errorf("sponsor: url: %w", err)
	}
	if s.BaseURL != "" {
		if err := validHTTPS(s.BaseURL); err != nil {
			return fmt.Errorf("sponsor: base url: %w", err)
		}
	}
	if s.StartsOn.IsZero() || s.EndsOn.IsZero() {
		return errors.New("sponsor: start and end dates are required")
	}
	if s.EndsOn.Before(s.StartsOn) {
		return errors.New("sponsor: end date is before start date")
	}
	return nil
}

// validHTTPS accepts only an absolute https URL with a host. The destination is
// put into a Location header and into an href, so it is checked at the door
// rather than trusted on the way out.
func validHTTPS(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.Hostname() == "" {
		return fmt.Errorf("%q is not an absolute https URL", raw)
	}
	return nil
}

// Active reports whether the sponsor is showing on a given UTC day.
func (s Sponsor) Active(day time.Time) bool {
	d := dateOnly(day)
	return !d.Before(dateOnly(s.StartsOn)) && !d.After(dateOnly(s.EndsOn))
}

func dateOnly(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ActiveSponsor returns the sponsor showing on the given day, or nil. Nil is the
// ordinary state, not an error: it is the house ad.
func ActiveSponsor(ctx context.Context, db *database.DB, day time.Time) (*Sponsor, error) {
	var sponsors []Sponsor
	err := db.Bun().NewSelect().
		Model(&sponsors).
		Where("starts_on <= ?::date AND ends_on >= ?::date", dateOnly(day).Format("2006-01-02"), dateOnly(day).Format("2006-01-02")).
		Order("starts_on DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	if len(sponsors) == 0 {
		return nil, nil
	}
	return &sponsors[0], nil
}

// ListSponsors returns every sponsor, newest first, for the management command.
func ListSponsors(ctx context.Context, db *database.DB) ([]Sponsor, error) {
	var sponsors []Sponsor
	err := db.Bun().NewSelect().Model(&sponsors).Order("starts_on DESC").Scan(ctx)
	return sponsors, err
}

// InsertSponsor validates and stores a sponsor. An overlap with an existing one
// is refused by the database's exclusion constraint and reported in words.
func InsertSponsor(ctx context.Context, db *database.DB, s Sponsor) (Sponsor, error) {
	s = s.Normalised()
	if err := s.Validate(); err != nil {
		return s, err
	}
	if _, err := db.Bun().NewInsert().Model(&s).Exec(ctx); err != nil {
		if strings.Contains(err.Error(), "sponsors_one_owner_at_a_time") {
			return s, errors.New("sponsor: those dates overlap an existing sponsor; end or delete that one first")
		}
		return s, err
	}
	return s, nil
}

// EndSponsorsOn ends every sponsor still running as of day by moving its end
// date to the day before, or deleting it if it has not started. It is how the
// slot is cleared early.
func EndSponsorsOn(ctx context.Context, db *database.DB, day time.Time) (int, error) {
	d := dateOnly(day).Format("2006-01-02")
	deleted, err := db.Bun().NewDelete().Model((*Sponsor)(nil)).
		Where("starts_on >= ?::date", d).Exec(ctx)
	if err != nil {
		return 0, err
	}
	removed, _ := deleted.RowsAffected()
	updated, err := db.Bun().NewUpdate().Model((*Sponsor)(nil)).
		Set("ends_on = ?::date - 1", d).
		Where("starts_on < ?::date AND ends_on >= ?::date", d, d).Exec(ctx)
	if err != nil {
		return int(removed), err
	}
	changed, _ := updated.RowsAffected()
	return int(removed + changed), nil
}

// SponsoredEntry is the API shape of the slot. It is its own type, with its own
// constant "sponsored": true, so that nothing that consumes a WorkingModel can
// be handed one.
type SponsoredEntry struct {
	Sponsored bool   `json:"sponsored"`
	Label     string `json:"label"`
	Notice    string `json:"notice"`
	Provider  string `json:"provider"`
	Pitch     string `json:"pitch"`
	// URL is the first-party tracked link, absolute. The destination is not
	// hidden -- its host is published beside it -- but the link a caller
	// follows is ours, so the sponsor can be told how many arrived.
	URL             string `json:"url"`
	DestinationHost string `json:"destination_host"`
	Model           string `json:"model,omitempty"`
	BaseURL         string `json:"base_url,omitempty"`
	StartsOn        string `json:"starts_on"`
	EndsOn          string `json:"ends_on"`
	// Probe is the real probe row for the sponsor's endpoint when it is on the
	// shelf, exactly as the models array would carry it. Null otherwise. It is
	// never synthesised.
	Probe *WorkingModel `json:"probe"`
}

// NewSponsoredEntry builds the API entry. origin is the public origin so the
// tracked link is absolute.
func NewSponsoredEntry(s Sponsor, probe *WorkingModel, origin string) *SponsoredEntry {
	return &SponsoredEntry{
		Sponsored:       true,
		Label:           sponsorLabel,
		Notice:          sponsorNotice,
		Provider:        s.Provider,
		Pitch:           s.Pitch,
		URL:             strings.TrimSuffix(origin, "/") + sponsorClickPath + "?src=" + clickSourceAPI,
		DestinationHost: sponsorHost(s),
		Model:           s.Model,
		BaseURL:         s.BaseURL,
		StartsOn:        s.StartsOn.UTC().Format("2006-01-02"),
		EndsOn:          s.EndsOn.UTC().Format("2006-01-02"),
		Probe:           probe,
	}
}

// ProbeFor finds the shelf row that is the sponsor's own endpoint, or nil. With
// a model named it must match that model; without one it takes the best-ranked
// row for the endpoint. A sponsor that names no slug, or one that is not on the
// shelf, gets nil -- and nil is rendered as "no probe data", never as a status.
func ProbeFor(s Sponsor, shelf []WorkingModel) *WorkingModel {
	if s.EndpointSlug == "" {
		return nil
	}
	for i := range shelf {
		if shelf[i].Slug != s.EndpointSlug {
			continue
		}
		if s.Model != "" && shelf[i].ModelID != s.Model {
			continue
		}
		row := shelf[i]
		return &row
	}
	return nil
}

// sponsorHost is the destination host for display.
func sponsorHost(s Sponsor) string {
	parsed, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// SponsorSlot is what the page renders: the active sponsor with its probe, or
// nothing, in which case the house ad shows.
type SponsorSlot struct {
	Sponsor *Sponsor
	Probe   *WorkingModel
}

// LoadSponsorSlot reads the slot for a day. Failure degrades to the house ad
// and is the caller's to log: a broken sponsor lookup must never take the shelf
// or the API down with it.
func LoadSponsorSlot(ctx context.Context, db *database.DB, day time.Time) (SponsorSlot, error) {
	sponsor, err := ActiveSponsor(ctx, db, day)
	if err != nil || sponsor == nil {
		return SponsorSlot{}, err
	}
	slot := SponsorSlot{Sponsor: sponsor}
	if sponsor.EndpointSlug != "" {
		shelf, err := WorkingModels(ctx, db, pageLimit, nil, AuthModeAny)
		if err != nil {
			return slot, err
		}
		slot.Probe = ProbeFor(*sponsor, shelf)
	}
	return slot, nil
}

// Normalised trims every text field. Both ways a sponsor arrives -- the
// management command and the purchase webhook -- go through it and then through
// Validate, so a slot can never be published under rules one of them skipped.
func (s Sponsor) Normalised() Sponsor {
	s.Provider = strings.TrimSpace(s.Provider)
	s.Pitch = strings.TrimSpace(s.Pitch)
	s.URL = strings.TrimSpace(s.URL)
	s.BaseURL = strings.TrimSpace(s.BaseURL)
	s.Model = strings.TrimSpace(s.Model)
	s.EndpointSlug = strings.TrimSpace(s.EndpointSlug)
	return s
}
