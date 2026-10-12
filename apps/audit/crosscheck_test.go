package audit

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// A reject list is only worth publishing if every line on it can be re-run or
// says why it cannot be. These assertions are that promise: a name, a target,
// and a verdict that is a measurement rather than an opinion.
func TestRejectedClaimsAreCheckable(t *testing.T) {
	if len(RejectedClaims) == 0 {
		t.Fatal("nothing is published as checked-and-rejected, which reads as nothing having been checked")
	}

	seen := map[string]bool{}
	for _, claim := range RejectedClaims {
		if claim.Name == "" || claim.Endpoint == "" || claim.Verdict == "" {
			t.Errorf("incomplete reject %+v: each one needs a name, what was called, and what came back", claim)
			continue
		}
		if seen[claim.Endpoint] {
			t.Errorf("%q is rejected twice", claim.Endpoint)
		}
		seen[claim.Endpoint] = true

		// A verdict that names no status code and no reason is an opinion, and
		// an opinion is exactly what this site exists not to publish. The
		// grouped entries -- scraped web UIs, vendor CLIs -- are the exception
		// on purpose: their reason is a licence, not a response.
		if !strings.HasPrefix(claim.Endpoint, "http") {
			continue
		}
		if !strings.ContainsAny(claim.Verdict, "0123456789") {
			t.Errorf("%s was rejected without citing a status code: %q", claim.Name, claim.Verdict)
		}
	}
}

// The rejects reach the page. They are folded away, which is a different thing
// from absent: the summary has to carry the count so the fold is worth opening.
func TestRejectedClaimsRenderOnTheShelf(t *testing.T) {
	query := ParseShelfQuery(url.Values{}).On(shelfPath)
	body := renderPage(t, HomePage(nil, 0, query, nil, "https://stillworks.supercapybara.com"))

	if !strings.Contains(body, "Checked and not listed") {
		t.Fatal("the shelf does not say anything was checked and rejected")
	}
	for _, claim := range RejectedClaims {
		if !strings.Contains(body, claim.Name) {
			t.Errorf("%q was rejected but never published", claim.Name)
		}
	}
}

// Nothing may be on both lists. A row on the shelf that is also published as
// rejected would leave a visitor with two answers and no way to choose.
func TestRejectedClaimsAreNotSeeded(t *testing.T) {
	for _, claim := range RejectedClaims {
		for _, endpoint := range SeedEndpoints {
			if endpoint.BaseURL != "" && strings.HasPrefix(claim.Endpoint, endpoint.BaseURL) {
				t.Errorf("%s is seeded as %q and also published as rejected", claim.Endpoint, endpoint.Slug)
			}
		}
	}
}

// The caption under the fold is built from the sweeps, so a sweep that is run
// and recorded cannot leave the page quoting an older catalogue size.
func TestCrosscheckCaptionNamesEverySweep(t *testing.T) {
	if len(CrosscheckSweeps) < 2 {
		t.Fatal("the second sweep is not recorded")
	}
	caption := crosscheckCaption()
	for _, sweep := range CrosscheckSweeps {
		if sweep.Providers <= 0 || sweep.Claims <= 0 || sweep.Claims > sweep.Providers {
			t.Errorf("sweep %s has impossible counts: %+v", sweep.Date, sweep)
		}
		if !strings.Contains(caption, sweep.Date) || !strings.Contains(caption, strconv.Itoa(sweep.Providers)) {
			t.Errorf("sweep %s is missing from the caption: %q", sweep.Date, caption)
		}
	}
}

// What the 2026-10-12 sweep added must be on the keyless shelf. Kilo lists 390
// models of which 18 are free, so it takes a larger chat share than the default.
func TestSecondSweepAdditionsAreSeededKeyless(t *testing.T) {
	want := map[string]bool{"kilo-gateway": false, "free-ai": false}
	for _, endpoint := range SeedEndpoints {
		if _, ok := want[endpoint.Slug]; !ok {
			continue
		}
		want[endpoint.Slug] = true
		if endpoint.AuthMode != AuthModeNone || endpoint.KeyEnv != "" {
			t.Errorf("%s must be keyless with no key variable", endpoint.Slug)
		}
		if endpoint.Slug == "kilo-gateway" && endpoint.ChatProbes <= defaultChatProbesPerCycle {
			t.Errorf("%s lists mostly paid models and needs a larger probe share", endpoint.Slug)
		}
	}
	for slug, found := range want {
		if !found {
			t.Errorf("%s was verified keyless on 2026-10-12 and is not seeded", slug)
		}
	}
}
