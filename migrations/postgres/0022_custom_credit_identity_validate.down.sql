DO $$ BEGIN
 RAISE EXCEPTION 'custom credit identity is a forward-only pre-launch cutover; restore the matching database/application snapshot to roll back';
END $$;
