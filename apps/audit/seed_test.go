package audit

import "testing"

// The keyed shelf shipped, was configured with real keys, and stayed blank in
// production because seeding only ran against an empty table. These assertions
// are about the seed data itself -- the row-level proof that an upsert reaches
// a non-empty database is in the PR -- and they guard the two properties that
// made the blank shelf possible: an endpoint on the key shelf must name the
// variable its key lives in, and no slug may appear twice.
func TestSeedClaimsAreConsistent(t *testing.T) {
	seen := map[string]bool{}
	keyed := 0
	for _, endpoint := range SeedEndpoints {
		if seen[endpoint.Slug] {
			t.Errorf("slug %q is seeded twice; the upsert would fight itself", endpoint.Slug)
		}
		seen[endpoint.Slug] = true

		switch endpoint.AuthMode {
		case AuthModeNone:
			if endpoint.KeyEnv != "" {
				t.Errorf("%s is on the keyless shelf but names a key variable %q", endpoint.Slug, endpoint.KeyEnv)
			}
		case AuthModeKey:
			keyed++
			if endpoint.KeyEnv == "" {
				// Without this the endpoint is probed bare and recorded as
				// needing a key -- published on the wrong shelf, silently.
				t.Errorf("%s is on the key shelf with no key variable named", endpoint.Slug)
			}
		default:
			t.Errorf("%s has auth mode %q, which no shelf renders", endpoint.Slug, endpoint.AuthMode)
		}
	}
	if keyed == 0 {
		t.Fatal("no key-required endpoint is seeded at all")
	}
}
