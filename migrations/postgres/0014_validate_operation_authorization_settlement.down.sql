-- PostgreSQL cannot mark a validated constraint NOT VALID again. The schema
-- change belongs to 0013, whose down migration removes the constraint and
-- columns; rolling back this validation-only step has no operation to apply.

SELECT 1;
