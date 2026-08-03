-- Providers list everything they serve under one /v1/models: OVH's 22 entries
-- include stable-diffusion, bge embeddings and whisper. Chat-probing those is a
-- guaranteed 4xx and burns requests against a 2-per-minute anonymous limit, so a
-- model that cannot answer a chat call is marked and skipped rather than retried
-- every cycle and recorded as a failure it never deserved.
ALTER TABLE llm_models
    ADD COLUMN chat_capable BOOLEAN NOT NULL DEFAULT TRUE;

-- Where a provider publishes modalities (Pollinations does), record them
-- verbatim so the judgement can be re-derived rather than trusted.
ALTER TABLE llm_models
    ADD COLUMN input_modalities TEXT NOT NULL DEFAULT '',
    ADD COLUMN output_modalities TEXT NOT NULL DEFAULT '';
