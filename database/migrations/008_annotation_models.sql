-- Operator registry that RANKS annotation models, so consumers can pick a
-- preferred summary while keeping every model's alternative.
--
-- message_annotations is keyed (message_id, model): each model already holds
-- its own annotation, so a message naturally carries multiple summaries — one
-- per model (e.g. a quality-first model on one GPU box and a fast model on
-- another). This table adds the PRIORITY metadata on top: which model's
-- annotation is the one to show by default, with the rest retained as
-- alternatives and full history across model versions.
--
-- Deliberately NOT a foreign key against message_annotations.model: a new GPU
-- box may write an annotation before its model is registered here, and a model
-- may be registered before it has annotated anything. epistula-api LEFT JOINs this
-- table at read time, treats an unregistered model as priority 0 (an
-- alternative), and orders a message's annotations by
-- (not retired, priority DESC, created_at DESC). A retired model's annotations
-- are still served as alternatives but are never the primary.
--
-- Lifecycle: `epistula-database admin annotation-model-{set,list,retire}`.

CREATE TABLE annotation_models (
    model        TEXT PRIMARY KEY,                -- matches message_annotations.model
    priority     INTEGER NOT NULL DEFAULT 0,      -- higher = preferred; ties broken by created_at DESC
    display_name TEXT,                            -- human label for UIs ("Qwen3.6 35B")
    retired_at   TIMESTAMPTZ,                     -- soft-retire: served as an alternative, never primary
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
