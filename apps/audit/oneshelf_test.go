package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

func boolPointer(value bool) *bool { return &value }

// keylessRow and keyedRow are the two kinds of row the one table now holds. The
// difference between them is the whole reason the auth chip exists, so every
// test below builds both rather than one and a variant.
func keylessRow() WorkingModel {
	verified := time.Now().Add(-20 * time.Minute)
	return WorkingModel{
		Slug:             "llm7",
		BaseURL:          "https://api.llm7.io",
		ChatPath:         "/v1/chat/completions",
		AuthMode:         AuthModeNone,
		ModelID:          "codestral-latest",
		ChatCapable:      true,
		OpenAICompatible: true,
		LastOK:           &verified,
		LatencyMS:        808,
		Attempts:         10,
		Successes:        8,
		Capabilities: map[string]CapabilityRecord{
			CapabilityTools:      {Supported: boolPointer(true)},
			CapabilityJSONSchema: {Supported: boolPointer(false), Claimed: boolPointer(true)},
			CapabilityJSONObject: {Claimed: boolPointer(true)},
			// vision is absent entirely: never probed, which is the fourth
			// state and is deliberately in none of the three arrays.
		},
	}
}

func keyedRow() WorkingModel {
	verified := time.Now().Add(-3 * time.Hour)
	return WorkingModel{
		Slug:             "groq",
		BaseURL:          "https://api.groq.com",
		ChatPath:         "/openai/v1/chat/completions",
		AuthMode:         AuthModeKey,
		ModelID:          "llama-3.3-70b-versatile",
		ChatCapable:      true,
		OpenAICompatible: true,
		LastOK:           &verified,
		LatencyMS:        183,
		Attempts:         2,
		Successes:        2,
		Capabilities:     map[string]CapabilityRecord{},
	}
}

// ?key= is the page's own control and the API's parameter, spelled the same,
// because the page tells visitors that its address is the API's. The two
// defaults differ on purpose and that difference is the point of this test: a
// person looking at a labelled table wants both kinds, and an agent that wrote
// /api/llm/up into its code before this parameter existed must keep receiving
// the keyless-only answer it was promised.
func TestTheKeyFilterIsOneParameterWithTwoDeliberateDefaults(t *testing.T) {
	for value, want := range map[string]string{
		AuthModeNone: AuthModeNone,
		AuthModeKey:  AuthModeKey,
		AuthModeAny:  AuthModeAny,
		"NONE":       AuthModeNone,
		" key ":      AuthModeKey,
	} {
		query := ParseShelfQuery(url.Values{keyParameter: []string{value}})
		if query.Key != want {
			t.Errorf("?key=%q parsed as %q, want %q", value, query.Key, want)
		}
		if query.AuthFilter() != want {
			t.Errorf("?key=%q filters on %q, want %q", value, query.AuthFilter(), want)
		}
		asked, err := requestedAuthMode(url.Values{keyParameter: []string{strings.TrimSpace(value)}})
		if err != nil {
			t.Errorf("the API refused ?key=%q: %v", value, err)
		} else if asked != want {
			t.Errorf("the API read ?key=%q as %q, want %q", value, asked, want)
		}
	}

	// Unasked: every row on the page, keyless only from the API.
	page := ParseShelfQuery(nil)
	if page.Key != "" || page.AuthFilter() != AuthModeAny {
		t.Fatalf("an unfiltered page asks for %q, want every row", page.AuthFilter())
	}
	if page.Filtered() {
		t.Fatal("an untouched page reports itself as filtered")
	}
	asked, err := requestedAuthMode(nil)
	if err != nil || asked != AuthModeNone {
		t.Fatalf("the API with no parameters asks for %q (%v), want keyless only", asked, err)
	}

	// The older spelling still works, because it was published first and is in
	// other people's scripts; key wins where a caller sends both.
	if asked, err = requestedAuthMode(url.Values{"auth": []string{AuthModeKey}}); err != nil || asked != AuthModeKey {
		t.Fatalf("?auth=key read as %q (%v)", asked, err)
	}
	both := url.Values{"auth": []string{AuthModeAny}, keyParameter: []string{AuthModeNone}}
	if asked, err = requestedAuthMode(both); err != nil || asked != AuthModeNone {
		t.Fatalf("with both spellings the API read %q, want key= to win", asked)
	}

	// A value we do not publish is a refusal that names the valid ones, not a
	// filter that quietly does nothing.
	if _, err = requestedAuthMode(url.Values{keyParameter: []string{"keyed"}}); err == nil {
		t.Fatal("?key=keyed was accepted")
	}
	// The page is more forgiving than the API on purpose: a stale link should
	// still show the shelf rather than an error page.
	if stale := ParseShelfQuery(url.Values{keyParameter: []string{"keyed"}}); stale.Key != "" {
		t.Fatalf("a mistyped ?key= became the filter %q instead of falling back", stale.Key)
	}
}

// The combined ordering. Both kinds of endpoint share one table now, so the
// default has to say which kind wins before it says anything about speed:
// without the tier a keyed row with a perfect record on our own credential
// sits above every endpoint anybody can call for nothing.
func TestTheCombinedOrderingPutsKeylessFirstThenReliabilityThenSpeed(t *testing.T) {
	fragment := ParseShelfQuery(nil).workingOrderBy()

	tier := strings.Index(fragment, "auth_mode = 'none'")
	rate := strings.Index(fragment, "successes")
	speed := strings.Index(fragment, "latency_ms")
	switch {
	case tier < 0 || rate < 0 || speed < 0:
		t.Fatalf("the default ordering is missing one of its three keys: %q", fragment)
	case !(tier < rate && rate < speed):
		t.Fatalf("the default ordering is not tier, then reliability, then speed: %q", fragment)
	}

	// Unmeasured sorts last on both measurements. "No latency recorded" is not
	// the fastest endpoint, and a row nobody has managed to check is not the
	// most reliable one.
	for _, expected := range []string{
		"successes::numeric / NULLIF(attempts, 0) DESC NULLS LAST",
		"NULLIF(latency_ms, 0) ASC NULLS LAST",
	} {
		if !strings.Contains(fragment, expected) {
			t.Errorf("the default ordering does not contain %q: %q", expected, fragment)
		}
	}

	// It is not a column, so there is nothing to flip. A hand-typed ?order=asc
	// cannot invert the shelf's own recommendation.
	inverted := ParseShelfQuery(url.Values{
		sortParameter:  []string{"best"},
		orderParameter: []string{orderAscending},
	})
	if inverted.workingOrderBy() != fragment {
		t.Fatalf("?sort=best&order=asc produced a different ordering: %q", inverted.workingOrderBy())
	}

	// Clicking a column takes over from it, and the sentence above the table
	// follows the sort rather than stating the default and being wrong.
	clicked := ParseShelfQuery(parseValues(t, ParseShelfQuery(nil).On(shelfPath).WorkingSortLink("latency")))
	if clicked.WorkingSort != "latency" || clicked.WorkingOrder != orderAscending {
		t.Fatalf("clicking Latency landed on %s/%s, want latency/asc", clicked.WorkingSort, clicked.WorkingOrder)
	}
	if sentence := orderingSentence(ParseShelfQuery(nil)); !strings.Contains(sentence, "No key needed first") {
		t.Fatalf("the default ordering sentence does not describe the default: %q", sentence)
	}
	if sentence := orderingSentence(clicked); strings.Contains(sentence, "No key needed first") {
		t.Fatalf("the ordering sentence still claims the default after a column was clicked: %q", sentence)
	}
}

// The API states its ordering rather than leaving it to be inferred. models[0]
// is a recommendation and walking the list down is the intended use, so what
// "down" means belongs on the payload -- and it has to be the same sentence the
// page prints above the same rows.
func TestRankedByNamesTheOrderingAndMatchesThePage(t *testing.T) {
	if keyless := rankedBy(AuthModeNone); strings.Contains(keyless, "no key needed first") {
		t.Fatalf("a keyless-only answer claims a tier it never applied: %q", keyless)
	}
	for _, auth := range []string{AuthModeAny, AuthModeKey} {
		if !strings.Contains(rankedBy(auth), "no key needed first") {
			t.Errorf("ranked_by for %s does not lead with the keyless tier: %q", auth, rankedBy(auth))
		}
	}
	for _, auth := range AuthModes {
		for _, part := range []string{"recent success rate", "latency"} {
			if !strings.Contains(rankedBy(auth), part) {
				t.Errorf("ranked_by for %s does not mention %s: %q", auth, part, rankedBy(auth))
			}
		}
	}
}

// The three flat arrays, and the fourth state that is deliberately not one of
// them. A feature missing from all three was never probed, which is a fact
// about us and not a verdict about the model -- publishing it as an
// "unprobed" array would invite it to be read as one.
func TestCapabilityArraysSeparateProvedFromClaimedFromFailed(t *testing.T) {
	model := keylessRow()

	assertNames(t, "proved", model.Proved(), []string{CapabilityTools})
	assertNames(t, "claimed_unproved", model.ClaimedUnproved(), []string{CapabilityJSONObject})
	assertNames(t, "failed", model.Failed(), []string{CapabilityJSONSchema})

	// vision is in none of them, and that is the answer.
	for name, names := range map[string][]string{
		"proved":           model.Proved(),
		"claimed_unproved": model.ClaimedUnproved(),
		"failed":           model.Failed(),
	} {
		for _, feature := range names {
			if feature == CapabilityVision {
				t.Fatalf("a never-probed feature reached %s", name)
			}
		}
	}

	// A model with no records at all still carries three arrays rather than
	// three nulls: the home page's field strip promises every model has them.
	empty := keyedRow()
	payload, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("marshalling a row failed: %v", err)
	}
	for _, field := range []string{`"proved":[]`, `"claimed_unproved":[]`, `"failed":[]`} {
		if !strings.Contains(string(payload), field) {
			t.Errorf("a row with no capability records is missing %s: %s", field, payload)
		}
	}
}

func assertNames(t *testing.T, field string, got []string, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}

// Every field the home page's own strip promises has to actually be on the
// wire. The strip is a claim about the payload, and this site's whole argument
// is that a claim nobody re-checks is a claim that goes stale.
func TestEveryModelCarriesWhatTheFieldStripPromises(t *testing.T) {
	for _, model := range []WorkingModel{keylessRow(), keyedRow()} {
		payload, err := json.Marshal(model)
		if err != nil {
			t.Fatalf("marshalling %s failed: %v", model.ModelID, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(payload, &decoded); err != nil {
			t.Fatalf("the row did not round-trip: %v", err)
		}
		for _, field := range []string{
			"model", "openai_base_url", "auth", "latency_ms", "answered", "last_ok",
			"proved", "claimed_unproved", "failed",
		} {
			if _, present := decoded[field]; !present {
				t.Errorf("%s is missing %s: %s", model.ModelID, field, payload)
			}
		}
		// auth is the HTTP client's word for it, not the database's.
		wantAuth := "none"
		if model.RequiresKey() {
			wantAuth = "bearer"
		}
		if decoded["auth"] != wantAuth {
			t.Errorf("%s carries auth %v, want %q", model.ModelID, decoded["auth"], wantAuth)
		}
		if decoded["answered"] != model.Answered() {
			t.Errorf("%s carries answered %v, want %q", model.ModelID, decoded["answered"], model.Answered())
		}
		// auth_mode stays alongside it. It is a published field, and callers
		// already read it.
		if decoded["auth_mode"] != model.AuthMode {
			t.Errorf("%s lost auth_mode: %s", model.ModelID, payload)
		}
	}
}

// The three published addresses that became one. They were indexed and they are
// in other people's chat logs, so they answer 301 rather than 404 -- and they
// carry their filters over, because a link to the keyless shelf narrowed to tool
// calling should land on those rows and not on a reset table.
func TestTheRetiredShelvesRedirectAndKeepTheirMeaning(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		from   string
		target string
	}{
		{name: "keyless", key: AuthModeNone, from: "/llm/", target: "/?key=none"},
		{name: "keyed", key: AuthModeKey, from: "/llm/keyed/", target: "/?key=key"},
		{name: "test call", key: "", from: "/llm/try?endpoint=llm7&model=x", target: "/"},
		{
			name:   "keyless, filtered",
			key:    AuthModeNone,
			from:   "/llm/?feature=tools&q=llama&sort=latency",
			target: "/?feature=tools&key=none&q=llama",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			redirectToShelf(test.key)(response, httptest.NewRequest(http.MethodGet, test.from, nil))
			if response.Code != http.StatusMovedPermanently {
				t.Fatalf("%s answered %d, want 301", test.from, response.Code)
			}
			if location := response.Header().Get("Location"); location != test.target {
				t.Fatalf("%s redirected to %q, want %q", test.from, location, test.target)
			}
		})
	}
}

// One table, both kinds of row, each labelled -- and a filter that is a link,
// so it works with the script switched off. The label is what replaced two
// separate addresses, so it is the thing that has to be on every row.
func TestTheOneShelfLabelsBothKindsOfRowAndNeverPrintsAKey(t *testing.T) {
	const secret = "gsk_thisisoursandmustneverappear"
	t.Setenv("GROQ_API_KEY", secret)

	query := ParseShelfQuery(nil).On(shelfPath)
	sweep := time.Now().Add(-21 * time.Minute)
	body := renderPage(t, HomePage(
		[]WorkingModel{keylessRow(), keyedRow()}, 2, query, &sweep, "https://stillworks.supercapybara.com"))

	if strings.Contains(body, secret) {
		t.Fatal("the shelf rendered our own provider key")
	}
	for _, wanted := range []string{
		`<span class="authchip none">not needed</span>`,
		`<span class="authchip key">key</span>`,
		"codestral-latest",
		"llama-3.3-70b-versatile",
	} {
		if !strings.Contains(body, wanted) {
			t.Errorf("the combined table is missing %q", wanted)
		}
	}

	// A keyed row gets no test button. A call from here would need either the
	// reader's key, which we will not ask for, or ours.
	keyedDrawer := section(body, `data-row-detail="groq:llama-3.3-70b-versatile"`)
	if strings.Contains(keyedDrawer, "data-try=") {
		t.Error("a key-required row offers a browser test call")
	}
	if !strings.Contains(keyedDrawer, "we will not ask for it") {
		t.Error("a key-required row does not say why it has no test button")
	}
	if !strings.Contains(keyedDrawer, "YOUR_GROQ_API_KEY") {
		t.Error("a key-required row does not mark the key as the reader's to supply")
	}
	keylessDrawer := section(body, `data-row-detail="llm7:codestral-latest"`)
	if !strings.Contains(keylessDrawer, "data-try=") {
		t.Error("a keyless row lost its browser test call")
	}
	// The snippet carries no credential, and the drawer says so in the strongest
	// form there is: not a placeholder, but that no header should be sent.
	if strings.Contains(keylessDrawer, "-H &#39;Authorization") {
		t.Error("a keyless snippet carries an Authorization header")
	}
	if !strings.Contains(keylessDrawer, "no Authorization header sent") {
		t.Error("a keyless row does not say that no credential is sent at all")
	}

	// Every control is an address. With no script the chips, the sort headings
	// and the carets are all links that work on a round trip.
	for _, wanted := range []string{`href="/?key=none"`, `href="/?key=key"`, `href="/?feature=tools"`} {
		if !strings.Contains(body, wanted) {
			t.Errorf("the controls are missing the link %q", wanted)
		}
	}
	if !strings.Contains(body, `href="/?open=llm7%3Acodestral-latest"`) {
		t.Error("the caret is not a link that opens the row on the server")
	}
	// Not one inline style or script: the policy is default-src 'self' and
	// drops both silently, so the page would look right locally and be wrong in
	// production.
	if strings.Contains(body, "<style") || strings.Contains(body, "<script") || strings.Contains(body, " style=") {
		t.Error("the page carries markup the Content-Security-Policy will drop")
	}
}

// A filter naming something we publish no verdict for keeps its warning. A
// filter that quietly does nothing is worse than one that refuses, because the
// table underneath it reads as an answer to a question nobody asked.
func TestAnUnknownFeatureStillWarnsOnTheOneShelf(t *testing.T) {
	query := ParseShelfQuery(url.Values{featureParameter: []string{"nonsense"}}).On(shelfPath)
	if len(query.UnknownFeatures) != 1 || query.UnknownFeatures[0] != "nonsense" {
		t.Fatalf("UnknownFeatures = %v, want nonsense", query.UnknownFeatures)
	}
	body := renderPage(t, HomePage(nil, 19, query, nil, "https://stillworks.supercapybara.com"))
	if !strings.Contains(body, "No verdict is published for nonsense") {
		t.Fatal("the page swallowed a filter it could not apply")
	}
	// And the shelf still renders: a mistyped filter is not an error page.
	if !strings.Contains(body, "stillworks") {
		t.Fatal("the page did not render at all")
	}
}

func renderPage(t *testing.T, component templ.Component) string {
	t.Helper()
	var out strings.Builder
	if err := component.Render(context.Background(), &out); err != nil {
		t.Fatalf("rendering failed: %v", err)
	}
	return out.String()
}

// section is the slice of HTML starting at a marker, so a test can ask about one
// row's drawer rather than about the whole document.
func section(body string, marker string) string {
	start := strings.Index(body, marker)
	if start < 0 {
		return ""
	}
	rest := body[start:]
	if end := strings.Index(rest, "</tr>"); end >= 0 {
		return rest[:end]
	}
	return rest
}

func parseValues(t *testing.T, link string) url.Values {
	t.Helper()
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link %q does not parse: %v", link, err)
	}
	return parsed.Query()
}
