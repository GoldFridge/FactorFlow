-- +goose Up

-- Words about a price, kept beside the price rather than inside it.
--
-- An assessment is immutable: it is what the model published and what the venue priced
-- against. A narration is not. It can arrive late, fail to arrive, be written by this
-- codebase from the stored contributions, or be replaced when a language model answers and
-- what it wrote survives checking. None of that may touch the priced record.
--
-- Source says who wrote it, and it is the field a reader should look at first: a sentence
-- from a model and a sentence derived from the coefficients carry different authority, and
-- a screen that showed them identically would be hiding the only thing that distinguishes
-- them.
CREATE TABLE assessment_explanations (
    assessment_id UUID PRIMARY KEY REFERENCES risk_assessments (id) ON DELETE CASCADE,
    source        TEXT        NOT NULL CHECK (source IN ('model', 'derived')),
    model         TEXT        NOT NULL DEFAULT '',
    bullets       JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,

    -- A derived explanation has no model behind it, and a model's has to name itself.
    CONSTRAINT assessment_explanations_model_named
        CHECK ((source = 'derived' AND model = '') OR (source = 'model' AND length(btrim(model)) > 0))
);

-- +goose Down
DROP TABLE IF EXISTS assessment_explanations;
