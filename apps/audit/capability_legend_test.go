package audit

import (
	"strings"
	"testing"
	"time"
)

// The columns are the site's only real differentiator, and a column nobody can
// read is noise wearing a differentiator's clothes. Handler had to ask what
// Schema meant, which is the visible half of every visitor asking it silently
// and leaving.
//
// So: the definition is on the page, in text, reachable from the column itself,
// with no JavaScript and no hover -- a phone has neither.
func TestEveryFeatureColumnSaysWhereItsDefinitionIs(t *testing.T) {
	body := renderPage(t, HomePage(
		[]WorkingModel{keylessRow(), keyedRow()}, 2, ParseShelfQuery(nil), nil, "https://stillworks.example"))

	if strings.Count(body, `href="#what-the-columns-mean"`) < 5 {
		t.Errorf("the four feature headings and the key under the table should all reach the legend, got %d links",
			strings.Count(body, `href="#what-the-columns-mean"`))
	}
	// The id is on the summary and not on the details, because a fragment
	// naming a closed details is not required to open it and one naming an
	// element inside it is. Getting this backwards sends a visitor to a panel
	// that stays shut, which is worse than no link.
	if !strings.Contains(body, `<summary id="what-the-columns-mean">`) {
		t.Error("the legend's anchor is not on the summary, so following it need not open the panel")
	}
	for _, heading := range []string{"Tools", "Schema", "JSON", "Vision"} {
		if !strings.Contains(body, `class="cap-head"`) || !strings.Contains(body, ">"+heading+"</a>") {
			t.Errorf("the %s heading is not a link to its own definition", heading)
		}
	}
	// Prose, not a title attribute. The title attribute stays for pointers, but
	// it cannot be the only copy of the answer.
	for _, phrase := range []string{
		"tool_calls",
		"json_schema",
		"json_object",
		"no, claimed",
		"never probed",
	} {
		if !strings.Contains(body, phrase) {
			t.Errorf("the legend never says %q", phrase)
		}
	}
}

// The same four words are also a filter, and a comma in a filter reads as "any
// of these" in most APIs a developer used this week. Here it means all of them,
// which changes what comes back, so the page has to say it where the columns
// are rather than in a README.
func TestTheLegendPrintsTheApiFormOfTheColumnsAndSaysTheyAreAnded(t *testing.T) {
	body := renderPage(t, HomePage(
		[]WorkingModel{keylessRow()}, 1, ParseShelfQuery(nil), nil, "https://stillworks.example"))

	if !strings.Contains(body, "/api/llm/up?feature=tools,json_schema") {
		t.Error("the legend does not print the API call that filters on the columns it defines")
	}
	if !strings.Contains(body, "<b>and</b>, never or") {
		t.Error("the legend never says that several features are ANDed")
	}

	// And the claim has to be true of the query the API actually runs.
	query := ParseShelfQuery(map[string][]string{featureParameter: {"tools,json_schema"}})
	if len(query.Features) != 2 {
		t.Fatalf("?feature=tools,json_schema parsed as %v", query.Features)
	}
}

// A verdict with no visible working is the thing this site exists to replace.
// The legend links each column to the call it was read from, and that call is
// on the endpoint's page under an anchor named after the capability.
func TestALegendTermLinksToTheCallItsVerdictWasReadFrom(t *testing.T) {
	when := time.Now().Add(-90 * time.Minute)
	evidence := []ProbeRow{
		{StartedAt: when, Kind: CapabilityTools, Outcome: OutcomeOK, HTTPStatus: 200, LatencyMS: 700, ModelUsed: "codestral-latest"},
		{StartedAt: when, Kind: CapabilityJSONSchema, Outcome: OutcomeClientError, HTTPStatus: 405, LatencyMS: 120, ModelUsed: "codestral-latest", Error: "json_schema not supported"},
	}
	body := renderPage(t, EndpointPage(endpointRow(), nil, nil, evidence))

	for _, capability := range []string{CapabilityTools, CapabilityJSONSchema} {
		if !strings.Contains(body, `href="/llm/llm7#probe-`+capability+`"`) {
			t.Errorf("the legend's %s term does not link to the call behind it", capability)
		}
		if !strings.Contains(body, `<tr id="probe-`+capability+`">`) {
			t.Errorf("nothing on the page carries the anchor the %s link points at", capability)
		}
	}
	// The 405 is evidence too. A page that published only its successes would
	// be every other free-LLM list.
	if !strings.Contains(body, "405") || !strings.Contains(body, "json_schema not supported") {
		t.Error("the refusal behind a no is not shown")
	}
	// Vision was never probed on this endpoint, so its term stays plain rather
	// than linking to an anchor that is not on the page.
	if strings.Contains(body, "#probe-"+CapabilityVision) {
		t.Error("a capability with no probe was still linked to evidence")
	}
}

// Same panel on the shelf, where no single endpoint is in scope. Every link it
// could print there would be a guess, so it prints none and says where the
// calls live instead.
func TestTheShelfLegendPrintsNoEvidenceLinkItCannotHonour(t *testing.T) {
	body := renderPage(t, HomePage(
		[]WorkingModel{keylessRow()}, 1, ParseShelfQuery(nil), nil, "https://stillworks.example"))

	if strings.Contains(body, "#probe-") {
		t.Error("the shelf's legend links to per-endpoint evidence anchors that are not on the shelf")
	}
	if !strings.Contains(body, "The call behind each verdict") {
		t.Error("the shelf's legend never says where the calls behind the verdicts are")
	}
}

// An endpoint with no feature probe yet says so, rather than rendering a table
// with no rows under a heading that promises evidence.
func TestAnEndpointWithNoFeatureProbeSaysThatRatherThanShowingAnEmptyTable(t *testing.T) {
	body := renderPage(t, EndpointPage(endpointRow(), nil, nil, nil))

	if !strings.Contains(body, "No feature probe has run against this endpoint yet") {
		t.Error("an endpoint with no feature evidence does not say so")
	}
	if strings.Contains(body, `<tr id="probe-`) {
		t.Error("an anchor was written for evidence that does not exist")
	}
}

// The evidence map is built from probe kinds, and the log holds kinds that are
// not capabilities. A chat probe is not evidence for a feature column and must
// not be linked as if it were.
func TestOnlyCapabilityProbesBecomeEvidenceLinks(t *testing.T) {
	links := capabilityEvidenceLinks("llm7", []ProbeRow{
		{Kind: KindChat},
		{Kind: CapabilityVision},
	})
	if _, held := links[KindChat]; held {
		t.Error("a chat probe was published as evidence for a feature column")
	}
	if links[CapabilityVision] != "/llm/llm7#probe-vision" {
		t.Errorf("the vision link is %q", links[CapabilityVision])
	}
}

func endpointRow() ShelfRow {
	return ShelfRow{
		Slug:             "llm7",
		Provider:         "llm7",
		BaseURL:          "https://api.llm7.io",
		ChatPath:         "/v1/chat/completions",
		AuthMode:         AuthModeNone,
		OpenAICompatible: true,
	}
}
