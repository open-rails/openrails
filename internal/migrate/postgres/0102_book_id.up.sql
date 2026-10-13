-- book_id is the billing book's logical identity: it survives a dump, a
-- restore and `openrails book arm`, which move only the physical location.
-- Every provider object OpenRails creates carries it, so an unknown object
-- naming this book is one the database lost and any other is foreign.
-- tagging_started_at is when this book began tagging; an untagged object
-- older than it predates the tag.
ALTER TABLE billing.book_identity
    ADD COLUMN book_id uuid DEFAULT uuidv7() NOT NULL,
    ADD COLUMN tagging_started_at timestamp with time zone DEFAULT now() NOT NULL;
COMMENT ON COLUMN billing.book_identity.book_id IS 'The book''s logical identity, written into every provider object OpenRails creates; kept by arm_book and by a dump and restore of the database.';
COMMENT ON COLUMN billing.book_identity.tagging_started_at IS 'When the book began tagging provider objects with book_id; an untagged object created before it predates the tag.';
