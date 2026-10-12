package audit

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/bon5co/godjango/database"
	"github.com/bon5co/godjango/management"
)

const sponsorUsage = `usage:
  manage sponsor list
  manage sponsor set  -provider NAME -pitch TEXT -url https://... -from YYYY-MM-DD -to YYYY-MM-DD
                      [-model ID] [-base-url https://...] [-endpoint SLUG]
  manage sponsor end                  end whoever holds the slot today (or delete one that has not started)
  manage sponsor report               audience figures and click counts, humans and agents apart`

// sponsorCommand is how the slot is changed without a deploy: whoever sells it
// runs one command against the production database.
func sponsorCommand(services management.ProjectServices) management.Command {
	return management.Command{
		Name:    "sponsor",
		Summary: "Show, set, end or report on the sponsored slot (list|set|end|report)",
		Run: func(ctx context.Context, args []string, streams management.Streams) error {
			if len(args) == 0 {
				return fmt.Errorf("%s", sponsorUsage)
			}
			return withDatabase(ctx, services, func(db *database.DB) error {
				return runSponsor(ctx, db, args[0], args[1:], streams.Out)
			})
		},
	}
}

func runSponsor(ctx context.Context, db *database.DB, verb string, args []string, out io.Writer) error {
	switch verb {
	case "list":
		sponsors, err := ListSponsors(ctx, db)
		if err != nil {
			return err
		}
		if len(sponsors) == 0 {
			fmt.Fprintln(out, "no sponsors -- the house ad is showing")
		}
		for _, s := range sponsors {
			state := "scheduled"
			switch {
			case s.Active(time.Now()):
				state = "ACTIVE"
			case dateOnly(s.EndsOn).Before(dateOnly(time.Now())):
				state = "ended"
			}
			fmt.Fprintf(out, "#%d %-9s %s .. %s  %s  %s\n", s.ID, state,
				s.StartsOn.Format("2006-01-02"), s.EndsOn.Format("2006-01-02"), s.Provider, s.URL)
		}
		return nil
	case "set":
		flags := flag.NewFlagSet("sponsor set", flag.ContinueOnError)
		flags.SetOutput(out)
		provider := flags.String("provider", "", "provider name (<= 80 characters)")
		pitch := flags.String("pitch", "", "one-line pitch (<= 200 characters)")
		link := flags.String("url", "", "destination, https; an affiliate or tracking link is fine")
		from := flags.String("from", "", "first day, YYYY-MM-DD (UTC)")
		to := flags.String("to", "", "last day, inclusive, YYYY-MM-DD (UTC)")
		model := flags.String("model", "", "optional model id to show")
		baseURL := flags.String("base-url", "", "optional base URL to show, https")
		endpoint := flags.String("endpoint", "", "optional slug of an endpoint on the shelf, to show its real probe data")
		if err := flags.Parse(args); err != nil {
			return err
		}
		starts, err := time.Parse("2006-01-02", *from)
		if err != nil {
			return fmt.Errorf("sponsor: -from must be YYYY-MM-DD: %w", err)
		}
		ends, err := time.Parse("2006-01-02", *to)
		if err != nil {
			return fmt.Errorf("sponsor: -to must be YYYY-MM-DD: %w", err)
		}
		stored, err := InsertSponsor(ctx, db, Sponsor{
			Provider: *provider, Pitch: *pitch, URL: *link, Model: *model, BaseURL: *baseURL,
			EndpointSlug: *endpoint, StartsOn: starts, EndsOn: ends,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "sponsor #%d stored: %s, %s .. %s\n", stored.ID, stored.Provider, *from, *to)
		return nil
	case "end":
		changed, err := EndSponsorsOn(ctx, db, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "ended or removed %d sponsor(s)\n", changed)
		return nil
	case "report":
		audience, err := LoadSponsorAudience(ctx, db, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "last %d days (human visitors come from Plausible and are not read here)\n", audience.Days)
		fmt.Fprintf(out, "API calls            %d (of which /api/llm/up %d)\n", audience.APICalls, audience.UpCalls)
		fmt.Fprintf(out, "API client-days      %d (avg %.1f/day)\n", audience.APIClientDays, audience.AvgDailyClients)
		fmt.Fprintf(out, "sponsor clicks       %d from the page (people), %d from the API (agents and scripts)\n",
			audience.ClicksFromPage, audience.ClicksFromAPI)
		return nil
	default:
		return fmt.Errorf("sponsor: unknown verb %q\n%s", verb, sponsorUsage)
	}
}
