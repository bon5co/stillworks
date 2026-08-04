ALTER TABLE llm_endpoints
    DROP COLUMN IF EXISTS image_models_path,
    DROP COLUMN IF EXISTS image_mode,
    DROP COLUMN IF EXISTS image_path;

DROP TABLE IF EXISTS llm_model_capabilities;
