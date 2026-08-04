-- Reverting re-inerts scheduled dunning, the managed Stripe webhook reconciler
-- and the alert-only catalog pull. Only do this alongside reverting the callers.
DROP FUNCTION IF EXISTS openrails.psp_rail_merchant_ids(text[], int);
DROP FUNCTION IF EXISTS openrails.due_dunning_merchant_ids(text[], timestamptz, int);
