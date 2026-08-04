package audit

import (
	"net/url"
	"strings"
	"testing"
)

// The only defence against a sort parameter reaching the database is that it
// never does: the key selects a fragment from a fixed map or it selects the
// default. These are the shapes somebody would actually send.
func TestSortKeysOutsideTheAllowListNeverReachTheQuery(t *testing.T) {
	hostile := []string{
		"latency; DROP TABLE llm_probes--",
		"latency, (SELECT pg_sleep(10))",
		"last_ok DESC",
		"1",
		"model_id) --",
		"'; DELETE FROM llm_endpoints; --",
		"latency\x00",
		"LATENCY ",
	}
	for _, attempt := range hostile {
		t.Run(attempt, func(t *testing.T) {
			query := ParseShelfQuery(url.Values{
				sortParameter:         []string{attempt},
				endpointSortParameter: []string{attempt},
			})
			if _, known := workingSorts[query.WorkingSort]; !known {
				t.Fatalf("WorkingSort = %q, which is not an allow-listed key", query.WorkingSort)
			}
			if _, known := endpointSorts[query.EndpointSort]; !known {
				t.Fatalf("EndpointSort = %q, which is not an allow-listed key", query.EndpointSort)
			}
			for _, fragment := range []string{query.workingOrderBy(), query.endpointOrderBy()} {
				if strings.Contains(fragment, "DROP") ||
					strings.Contains(fragment, "DELETE") ||
					strings.Contains(fragment, "pg_sleep") ||
					strings.Contains(fragment, ";") ||
					strings.Contains(fragment, "--") {
					t.Fatalf("ORDER BY fragment carries input: %q", fragment)
				}
			}
		})
	}
}

// "LATENCY " is trimmed and lowercased into a real key rather than thrown away:
// a link that survived a mail client should still sort.
func TestSortKeysAreCaseAndSpaceInsensitive(t *testing.T) {
	query := ParseShelfQuery(url.Values{
		sortParameter:  []string{" Latency "},
		orderParameter: []string{"DESC"},
	})
	if query.WorkingSort != "latency" || query.WorkingOrder != orderDescending {
		t.Fatalf("sort = %q order = %q, want latency/desc", query.WorkingSort, query.WorkingOrder)
	}
}

// Every fragment in both tables has to be reachable and has to name a real
// output column. A key added to the map with a typo in its SQL would otherwise
// only fail on the request that used it.
func TestEverySortFragmentNamesAKnownColumn(t *testing.T) {
	// sort_value is a real output column too, but only for a key that brings the
	// expression producing it. The two have to travel together: a fragment
	// naming it without one would order by a column the select never emitted,
	// and an expression nobody orders by would be dead weight in every query.
	workingColumns := []string{"model_id", "slug", "latency_ms", "successes", "attempts", "last_ok", "sort_value"}
	endpointColumns := []string{"slug", "latency_ms", "checked_at", "models_listed", "working_models"}
	check := func(t *testing.T, fragments map[string]sortColumn, columns []string) {
		for key, column := range fragments {
			for _, fragment := range []string{column.ascending, column.descending} {
				if !mentionsAny(fragment, columns) {
					t.Errorf("%s: fragment %q names no known column", key, fragment)
				}
				if strings.Contains(fragment, "sort_value") != (column.selectExpression != "") {
					t.Errorf("%s: fragment %q and its select expression disagree about sort_value", key, fragment)
				}
			}
			if column.defaultOrder != orderAscending && column.defaultOrder != orderDescending {
				t.Errorf("%s: default order %q is neither asc nor desc", key, column.defaultOrder)
			}
		}
	}
	t.Run("working", func(t *testing.T) { check(t, workingSorts, workingColumns) })
	t.Run("endpoints", func(t *testing.T) { check(t, endpointSorts, endpointColumns) })
}

func mentionsAny(fragment string, columns []string) bool {
	for _, column := range columns {
		if strings.Contains(fragment, column) {
			return true
		}
	}
	return false
}

// A bare ?sort=key has to pick the direction a person meant. Fastest first for
// latency, newest first for a timestamp; one global default would be wrong for
// half the columns.
func TestBareSortKeyPicksTheUsefulDirection(t *testing.T) {
	for key, want := range map[string]string{
		"latency":     orderAscending,
		"verified":    orderDescending,
		"reliability": orderDescending,
		"model":       orderAscending,
	} {
		query := ParseShelfQuery(url.Values{sortParameter: []string{key}})
		if query.WorkingOrder != want {
			t.Errorf("?sort=%s gave order %q, want %q", key, query.WorkingOrder, want)
		}
	}
}

// Clicking the column you are already sorted by flips it; clicking another one
// starts at that column's own default rather than inheriting a direction that
// means nothing there.
func TestSortLinkFlipsTheActiveColumnAndResetsOthers(t *testing.T) {
	query := ParseShelfQuery(url.Values{
		sortParameter:  []string{"latency"},
		orderParameter: []string{orderAscending},
	})

	flipped := parseLink(t, query.WorkingSortLink("latency"))
	if flipped.Get(sortParameter) != "latency" || flipped.Get(orderParameter) != orderDescending {
		t.Fatalf("clicking the active column gave %v, want latency/desc", flipped)
	}

	// Clicking back to the default column produces the bare address, because
	// that address already means verified-descending. What matters is that
	// following the link lands in that state, not that the parameters are spelt
	// out in it.
	other := ParseShelfQuery(parseLink(t, query.WorkingSortLink("verified")))
	if other.WorkingSort != "verified" || other.WorkingOrder != orderDescending {
		t.Fatalf("clicking another column landed on %s/%s, want verified/desc", other.WorkingSort, other.WorkingOrder)
	}

	// And a non-default column does spell itself out, so the link survives.
	reliability := ParseShelfQuery(parseLink(t, query.WorkingSortLink("reliability")))
	if reliability.WorkingSort != "reliability" || reliability.WorkingOrder != orderDescending {
		t.Fatalf("reliability link landed on %s/%s", reliability.WorkingSort, reliability.WorkingOrder)
	}

	if mark := query.WorkingSortMark("latency"); mark == "" {
		t.Fatal("the active column carries no direction mark")
	}
	if mark := query.WorkingSortMark("model"); mark != "" {
		t.Fatalf("an inactive column is marked %q", mark)
	}
}

// A sort link that dropped the filters would silently widen the table under
// somebody who had just narrowed it.
func TestSortLinksCarryTheFiltersAlong(t *testing.T) {
	query := ParseShelfQuery(url.Values{
		endpointParameter: []string{"pollinations"},
		searchParameter:   []string{"gpt"},
		keylessParameter:  []string{"yes"},
	})
	link := parseLink(t, query.WorkingSortLink("latency"))
	for parameter, want := range map[string]string{
		endpointParameter: "pollinations",
		searchParameter:   "gpt",
		keylessParameter:  "yes",
	} {
		if link.Get(parameter) != want {
			t.Errorf("sort link lost %s: %v", parameter, link)
		}
	}
	// And the other table's sort is untouched by this table's link.
	if link.Get(endpointSortParameter) != "" {
		t.Errorf("a working-table sort link moved the endpoint table: %v", link)
	}
}

// The unfiltered shelf is / and nothing else. A default that travels in the URL
// makes every shared link outlive the default it was written against.
func TestDefaultStateHasACleanAddress(t *testing.T) {
	if address := ParseShelfQuery(nil).URL(); address != "/" {
		t.Fatalf("default URL = %q, want /", address)
	}
	if ParseShelfQuery(nil).Filtered() {
		t.Fatal("an untouched shelf reports itself as filtered")
	}
}

func TestKeylessFilterOnlyAcceptsTheTwoAnswers(t *testing.T) {
	for value, want := range map[string]string{
		"yes":      keylessOnly,
		"YES":      keylessOnly,
		"no":       keylessExclude,
		"maybe":    "",
		"1":        "",
		"' OR 1=1": "",
	} {
		if got := ParseShelfQuery(url.Values{keylessParameter: []string{value}}).Keyless; got != want {
			t.Errorf("keyless=%q parsed to %q, want %q", value, got, want)
		}
	}
}

// Filter text is bounded before it reaches an ILIKE or the page. Unbounded, one
// request becomes a large scan and a large echo back into the HTML.
func TestFilterTextIsBounded(t *testing.T) {
	long := strings.Repeat("g", filterTextLimit*3)
	query := ParseShelfQuery(url.Values{
		searchParameter:   []string{long},
		endpointParameter: []string{long},
	})
	if len(query.Search) != filterTextLimit || len(query.Endpoint) != filterTextLimit {
		t.Fatalf("search=%d endpoint=%d characters, want %d", len(query.Search), len(query.Endpoint), filterTextLimit)
	}
}

// A visitor searching for "%" means the character, not "everything". Leaving it
// unescaped makes the filter look broken in the one case where somebody is
// deliberately testing it.
func TestSearchWildcardsAreLiterals(t *testing.T) {
	for _, input := range []string{"%", "_", `\`, "gpt%"} {
		pattern := escapeLikePattern(input)
		if !strings.HasPrefix(pattern, "%") || !strings.HasSuffix(pattern, "%") {
			t.Fatalf("pattern %q is not a contains-match", pattern)
		}
		inner := pattern[1 : len(pattern)-1]
		if strings.Contains(strings.ReplaceAll(inner, `\%`, ""), "%") {
			t.Errorf("unescaped %% survived in %q", pattern)
		}
		if strings.Contains(strings.ReplaceAll(inner, `\_`, ""), "_") {
			t.Errorf("unescaped _ survived in %q", pattern)
		}
	}
}

func parseLink(t *testing.T, link string) url.Values {
	t.Helper()
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("sort link %q does not parse: %v", link, err)
	}
	if parsed.Path != "/" {
		t.Fatalf("sort link points at %q, want /", parsed.Path)
	}
	return parsed.Query()
}

// The page's own prose tells visitors to filter by capability. The HTML parsed
// ?feature=, discarded it, and returned every row -- so ?feature=tools read as
// "all twenty-three of these do tool calling". Parsing has to keep it.
func TestFeatureFilterSurvivesParsing(t *testing.T) {
	query := ParseShelfQuery(url.Values{
		featureParameter: []string{"tools,json_schema", "vision"},
	})
	want := []string{CapabilityTools, CapabilityJSONSchema, CapabilityVision}
	if len(query.Features) != len(want) {
		t.Fatalf("Features = %v, want %v", query.Features, want)
	}
	for index, name := range want {
		if query.Features[index] != name {
			t.Fatalf("Features = %v, want %v", query.Features, want)
		}
	}
	if len(query.UnknownFeatures) != 0 {
		t.Fatalf("UnknownFeatures = %v, want none", query.UnknownFeatures)
	}
	if !query.Filtered() {
		t.Fatal("a shelf narrowed to three capabilities does not consider itself filtered")
	}
}

// A name we publish no verdict for is kept rather than dropped, so the page can
// say the filter did not apply. Silently ignoring it is the behaviour being
// fixed; silently matching nothing would be worse still, because "no model
// supports tols" is a true sentence that sends somebody hunting an outage.
func TestUnknownFeaturesAreKeptSoThePageCanSaySo(t *testing.T) {
	query := ParseShelfQuery(url.Values{
		featureParameter: []string{"tools", "tols", "image_out", "tols"},
	})
	if len(query.Features) != 1 || query.Features[0] != CapabilityTools {
		t.Fatalf("Features = %v, want just tools", query.Features)
	}
	// image_out is refused with the typo: it belongs to models that draw, and
	// the working table holds models that chat, so it could only ever empty it.
	if len(query.UnknownFeatures) != 2 {
		t.Fatalf("UnknownFeatures = %v, want tols and image_out once each", query.UnknownFeatures)
	}
}

// The filter has to survive a round trip through its own URL, or a second
// filter silently drops the first.
func TestFeatureFilterRoundTripsThroughTheURL(t *testing.T) {
	first := ParseShelfQuery(url.Values{featureParameter: []string{"tools", "vision"}}).On(keylessShelfPath)
	parsed, err := url.Parse(first.URL())
	if err != nil {
		t.Fatalf("URL() produced something unparseable: %v", err)
	}
	second := ParseShelfQuery(parsed.Query())
	if len(second.Features) != 2 || !second.HasFeature(CapabilityTools) || !second.HasFeature(CapabilityVision) {
		t.Fatalf("round trip lost the filter: %v -> %q -> %v", first.Features, first.URL(), second.Features)
	}
}

// Every capability column is sortable, and the sort a heading links to is one
// the parser accepts. A heading pointing at a key ParseShelfQuery rejects would
// silently fall back to the default and look like a dead control.
func TestEveryCapabilityColumnHasAWorkingSort(t *testing.T) {
	for _, capability := range ChatCapabilities {
		query := ParseShelfQuery(url.Values{sortParameter: []string{capability}})
		if query.WorkingSort != capability {
			t.Errorf("?sort=%s parsed as %q", capability, query.WorkingSort)
		}
		if query.workingSortValue() == "" {
			t.Errorf("%s sorts on sort_value but supplies no expression to produce it", capability)
		}
	}
}

// The shelf's default order decides the top row, and the top row is the one
// most visitors will paste. Freshness churned -- it changed twice in ninety
// seconds during a review -- and plain reliability, once both kinds of endpoint
// shared one table, put a row that needs a signup above rows anybody can call
// for nothing. The default is neither: it is a tier, then reliability, then
// speed, and it is not a column anybody can click away.
func TestTheShelfDefaultsToKeylessFirstAndNotToAnyColumn(t *testing.T) {
	query := ParseShelfQuery(nil)
	if query.WorkingSort != "best" {
		t.Fatalf("default working sort = %q, want best", query.WorkingSort)
	}
	if query.WorkingOrder != orderDescending {
		t.Fatalf("default working order = %q, want desc", query.WorkingOrder)
	}
	// Both directions are the same fragment, so a hand-typed ?order=asc cannot
	// invert the shelf's own recommendation.
	ascending := ParseShelfQuery(url.Values{
		sortParameter:  []string{"best"},
		orderParameter: []string{orderAscending},
	})
	if ascending.workingOrderBy() != query.workingOrderBy() {
		t.Fatalf("?order=asc changed the default ordering to %q", ascending.workingOrderBy())
	}
	// No key needed is the first thing it sorts on, ahead of any measurement.
	fragment := query.workingOrderBy()
	if !strings.HasPrefix(fragment, "CASE WHEN auth_mode = 'none'") {
		t.Fatalf("the default ordering does not lead with the keyless tier: %q", fragment)
	}
	if strings.Index(fragment, "successes") > strings.Index(fragment, "latency_ms") {
		t.Fatalf("the default ordering puts speed ahead of reliability: %q", fragment)
	}
}
