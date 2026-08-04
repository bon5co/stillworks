-- "Does it answer without a key" is the first question an agent asks and the
-- last one every free-LLM list answers. The second question decides whether the
-- endpoint is usable at all: can this model call a tool, hold a schema, read an
-- image. Providers publish those answers themselves -- llm7 ships a
-- capabilities{} block per model, Pollinations ships tools/vision booleans --
-- and the answers are not always true. Verified on 2026-08-04 from this server:
-- OVH's Qwen2.5-VL-72B-Instruct is listed beside models that do tool calls and
-- replies 400 "feature 'tool calls' is not currently supported", and llm7's
-- meta-Llama-3.1-8B-Instruct-Turbo claims json_mode true while a json_schema
-- request comes back 405 upstream_client_error.
--
-- So the claim and the verdict are stored side by side. claimed is what the
-- provider says; supported is what a real call proved. Both are tri-state and
-- NULL means nobody said anything, which is never the same as "no".
CREATE TABLE llm_model_capabilities (
    id           SERIAL PRIMARY KEY,
    model_id     INTEGER NOT NULL REFERENCES llm_models(id) ON DELETE CASCADE,
    capability   TEXT NOT NULL,
    claimed      BOOLEAN,
    supported    BOOLEAN,
    last_checked TIMESTAMPTZ,
    last_ok      TIMESTAMPTZ,
    -- Why the last attempt did not prove support. Kept next to the verdict so a
    -- "no" can be read rather than trusted: "does not support vision input" and
    -- "the anonymous pool was empty" are both failures and only one of them is
    -- a fact about the model.
    last_error   TEXT NOT NULL DEFAULT '',
    UNIQUE (model_id, capability)
);

CREATE INDEX idx_llm_model_capabilities_model ON llm_model_capabilities(model_id);
CREATE INDEX idx_llm_model_capabilities_supported
    ON llm_model_capabilities(capability, supported);

-- Image generation is not reachable through the chat path, so the endpoint has
-- to say where it lives and in which shape. Two shapes are actually served:
-- OVH answers a POST /v1/images/generations with an OpenAI images envelope
-- (b64_json, 2.5 MB at its only supported size), Pollinations answers a plain
-- GET on a prompt URL with the image bytes themselves.
ALTER TABLE llm_endpoints
    ADD COLUMN image_path TEXT NOT NULL DEFAULT '',
    ADD COLUMN image_mode TEXT NOT NULL DEFAULT '',
    -- Some providers list their image models somewhere other than the model
    -- listing we already read: text.pollinations.ai/models has one text model
    -- and no image models at all, while image.pollinations.ai/models has the
    -- image model. Without this the image surface could only be claimed, never
    -- attributed to a model and verified.
    ADD COLUMN image_models_path TEXT NOT NULL DEFAULT '';
