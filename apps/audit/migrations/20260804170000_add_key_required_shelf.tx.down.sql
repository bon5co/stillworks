-- Retire the keyed endpoints before removing the machinery that keeps them
-- apart from the keyless ones. Dropping the columns alone would leave three
-- rows marked active with auth_mode 'key' in a schema whose code has no idea
-- what that means: the older prober has no key to send and no reason to skip
-- them, so it would call Groq, Mistral and OpenRouter bare, record the 401s as
-- keyless = false, and list all three under a heading that reads "Keyless only.
-- No key, no signup." A rollback must not publish the exact claim this
-- migration exists to prevent.
--
-- They are deactivated rather than deleted so their probe history survives.
UPDATE llm_endpoints SET active = FALSE WHERE auth_mode = 'key';

ALTER TABLE llm_endpoints
    DROP COLUMN IF EXISTS chat_probes_per_cycle;

DROP INDEX IF EXISTS idx_llm_models_answered_with_key;

ALTER TABLE llm_models
    DROP COLUMN IF EXISTS answered_with_key;

ALTER TABLE llm_endpoints
    DROP CONSTRAINT IF EXISTS llm_endpoints_auth_mode_known;

ALTER TABLE llm_endpoints
    DROP COLUMN IF EXISTS key_env;
