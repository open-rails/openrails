-- PostgreSQL cannot mark a validated constraint NOT VALID again. The schema
-- change itself belongs to 0010, whose down migration restores the old FK;
-- rolling back this validation-only step therefore has no operation to apply.

SELECT 1;
