DO $$ BEGIN
 RAISE EXCEPTION 'bound merchant name authority is a forward-only cutover; restore the matching application/database snapshot to roll back';
END $$;
