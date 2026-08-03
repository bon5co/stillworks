ALTER TABLE llm_models
    DROP COLUMN IF EXISTS output_modalities,
    DROP COLUMN IF EXISTS input_modalities,
    DROP COLUMN IF EXISTS chat_capable;
