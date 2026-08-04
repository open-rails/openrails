DROP INDEX IF EXISTS openrails.idx_rail_intents_destructive_run;
ALTER TABLE openrails.rail_intents DROP CONSTRAINT IF EXISTS rail_intents_destructive_run_fk;
ALTER TABLE openrails.rail_intents DROP COLUMN IF EXISTS destructive_run_id;
DROP TABLE IF EXISTS openrails.destructive_run_before_images;
