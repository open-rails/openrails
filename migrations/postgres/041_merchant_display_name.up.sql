-- A merchant carries a human display name (e.g. "Doujins Media") for end-user
-- surfaces and invoices, distinct from its slug. The slug stays the unique,
-- URL-safe, human-readable handle used in API paths and resolution (mutable; the
-- uuid remains the stored identity that FKs reference). display_name is free
-- text, non-unique, and optional — NULL falls back to the slug for display.
ALTER TABLE openrails.merchants ADD COLUMN display_name text;

COMMENT ON COLUMN openrails.merchants.display_name IS 'human-readable merchant name for end-user display / invoices; NULL = fall back to slug.';
