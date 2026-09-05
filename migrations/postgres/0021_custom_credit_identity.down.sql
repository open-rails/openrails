DO $$ BEGIN RAISE EXCEPTION 'custom credit identity is a forward-only cutover; restore the matching pre-launch database snapshot to roll back'; END $$;
