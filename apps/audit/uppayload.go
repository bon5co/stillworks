package audit

import "time"

// upPayload is the body of GET /api/llm/up, built from values alone so a test
// can hold it to its invariants without a database.
//
// The invariant that matters: working goes into "models" untouched and in the
// order it arrived. Callers take models[0] as the pick and models[1:] as the
// fallback chain.
func upPayload(
	working []WorkingModel,
	auth string,
	features []string,
	now time.Time,
) map[string]any {
	payload := map[string]any{
		"generated_at": now,
		"count":        len(working),
		// The ordering, stated rather than left to be inferred from the array.
		// models[0] is a recommendation and a caller walking the list down is
		// the intended use, so what "down" means has to be on the payload --
		// the page says the same sentence above the same rows.
		"ranked_by": rankedBy(auth),
		// Stated in the payload so a consumer cannot mistake this for a
		// guarantee about somebody else's free service. The keyless wording is
		// left exactly as it was: it is the payload a caller predating the
		// second shelf receives, and it is the stronger of the two claims.
		"disclaimer": disclaimerFor(auth),
		"models":     working,
	}
	if auth != AuthModeNone {
		// Echoed back, and qualified. A caller that opted in has to be told what
		// it opted into, on the envelope as well as on each row.
		payload["key"] = auth
		payload["auth"] = auth
		payload["auth_disclaimer"] = keyNote + " Every entry carries auth: \"none\" needs no " +
			"Authorization header at all, \"bearer\" needs one of your own. Requesting key=none, or " +
			"omitting the parameter entirely, returns keyless entries only."
	}
	if len(features) > 0 {
		// Echoed back so a caller can see its filter was understood rather than
		// silently dropped. The key is absent when nothing was asked for, so
		// the payload a caller predating the filter receives is unchanged.
		payload["features"] = features
		payload["feature_disclaimer"] = "A feature is listed only where a real call proved it: a tool call the " +
			"caller could dispatch, a reply that parsed and matched the schema, an answer about an image we sent. " +
			"Models whose features have never been probed are not returned by a filtered request."
	}
	return payload
}
