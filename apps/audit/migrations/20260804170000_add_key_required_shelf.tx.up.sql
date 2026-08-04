-- A second shelf, kept deliberately apart from the first.
--
-- An agent spent 2026-08-04 checking every plausible keyless LLM endpoint that
-- anybody has published and found no new ones. The genuinely keyless universe
-- is about four providers; "free tier" almost always means "free once you mint
-- a key". So the shelf that made this site worth reading cannot grow, and the
-- thing that actually made it worth reading was never the word keyless -- it
-- was that we re-probe and publish what happened.
--
-- That promise extends to providers who want a key. What must not extend is the
-- claim: an endpoint we reach with our own key proves that OUR key works, on
-- OUR account, today. It says nothing about what a new signup gets. The two
-- facts therefore never share a column, a table row, or a page.

-- auth_mode already exists and already means 'none' = no key and no signup.
-- 'key' is the second value: we hold a free-tier key for this provider in an
-- environment variable and every probe against it carries that key.
--
-- key_env is the NAME of that variable, never the value. The database is
-- dumped, backed up and read by anything with a psql prompt; a secret that
-- lives only in the process environment cannot leak through any of that. An
-- endpoint whose variable is unset is simply not probed -- see the Go side,
-- where that is an absence of rows rather than a failed one, because recording
-- "down" for a provider we never called would be a lie about somebody else's
-- service.
ALTER TABLE llm_endpoints
    ADD COLUMN key_env TEXT NOT NULL DEFAULT '';

-- Two values today. The constraint exists so that a third one has to be
-- introduced deliberately, in a migration, alongside the Go code that knows how
-- to send it -- rather than arriving as a typo in a seed row that silently
-- makes an endpoint unprobeable.
ALTER TABLE llm_endpoints
    ADD CONSTRAINT llm_endpoints_auth_mode_known CHECK (auth_mode IN ('none', 'key'));

-- The verdict a keyed probe produces, kept in its own column rather than
-- folded into keyless.
--
-- keyless means: this model answered a request that carried no Authorization
-- header. Only a probe that sent no header may ever write it. If a call made
-- with our Groq key could set keyless = true, the flagship shelf would fill up
-- with models that need a key, published under a heading that promises they do
-- not -- which is precisely the failure every other free-LLM list already
-- commits, just arrived at from the other direction.
--
-- Tri-state for the same reason keyless is. NULL means we have never asked with
-- a key, which is not the same as a refusal.
ALTER TABLE llm_models
    ADD COLUMN answered_with_key BOOLEAN;

-- The keyed shelf reads this the way the keyless one reads keyless.
CREATE INDEX idx_llm_models_answered_with_key ON llm_models(answered_with_key);

-- How many chat probes one endpoint may take per cycle.
--
-- Until now this was a single constant of 3 for everybody, and it was already
-- wrong for one endpoint: the OVH seed row's own note says "a single probe per
-- cycle is the ceiling of what is polite" against its 2-per-minute anonymous
-- limit, and the code then took three anyway. The second shelf makes it wrong
-- again and harder: OpenRouter's free tier allows a $0-balance account 50
-- requests a day across its :free models, and three an hour is 72 before the
-- capability cycle asks for anything.
--
-- Exceeding a limit a provider has published, on a tier they are giving away,
-- is not a bug that shows up in our output -- it shows up as 429s that we then
-- publish as if they were the provider's problem. So the budget belongs to the
-- endpoint, next to the note explaining it.
ALTER TABLE llm_endpoints
    ADD COLUMN chat_probes_per_cycle INTEGER NOT NULL DEFAULT 3
        CHECK (chat_probes_per_cycle BETWEEN 1 AND 10);
