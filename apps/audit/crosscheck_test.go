package audit

import (
	"net/url"
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
