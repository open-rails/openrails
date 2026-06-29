-- A product's identity is its key, not a "slug". The manifest already calls it
-- `key`; this renames the storage column to match so the term is consistent end
-- to end. Hard cut — no alias column, no view. PostgreSQL rewrites the unique
-- constraint definition onto the new column automatically on RENAME; the
-- constraint is also renamed so its name stops saying "slug".
--
-- The product key VALUE is unchanged (e.g. "premium" stays "premium"), so every
-- content-derived provider key (Stripe lookup_key, NMI plan id, Solana plan PDA)
-- is byte-identical — this is a pure rename, no provider drift.
ALTER TABLE openrails.products RENAME COLUMN slug TO key;
ALTER TABLE openrails.products RENAME CONSTRAINT products_merchant_slug_key TO products_merchant_key_key;
