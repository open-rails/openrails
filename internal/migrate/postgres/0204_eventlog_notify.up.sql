-- The shared event log's flush notifies "billing.eventlog" at commit (Helpers #16), so a
-- consumer reads when a stream moves instead of on a clock.

-- BEGIN helpers/eventlog/postgres schema v2 (verbatim)
-- Version 2: the flush notifies "<schema>.eventlog" with "<source> <scope>" for each stream it advances.
CREATE OR REPLACE FUNCTION "billing".eventlog_flush() RETURNS trigger LANGUAGE plpgsql SET search_path TO pg_catalog,pg_temp AS $fn$
DECLARE
 s record; e record; next_position bigint; stamp timestamptz;
BEGIN
 -- Every scope touched by this transaction is known before taking any locks.
 FOR s IN SELECT DISTINCT source,scope FROM "billing".eventlog_pending
          WHERE transaction_id=pg_current_xact_id() ORDER BY source,scope
 LOOP
  INSERT INTO "billing".eventlog_streams(source,scope) VALUES(s.source,s.scope) ON CONFLICT DO NOTHING;
  PERFORM 1 FROM "billing".eventlog_streams WHERE source=s.source AND scope=s.scope FOR UPDATE;
  -- Delivered at commit, so a LISTENing consumer reads when its stream has moved (version 2).
  PERFORM pg_notify(TG_TABLE_SCHEMA || '.eventlog', s.source || ' ' || s.scope);
 END LOOP;
 FOR e IN SELECT * FROM "billing".eventlog_pending WHERE transaction_id=pg_current_xact_id() ORDER BY ordinal
 LOOP
  IF EXISTS(SELECT 1 FROM "billing".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id) THEN
   IF EXISTS(SELECT 1 FROM "billing".eventlog_events WHERE source=e.source AND scope=e.scope AND id=e.id
     AND event_source=e.event_source AND event_type=e.event_type AND version=e.version
     AND occurred_at=e.occurred_at AND subject_type=e.subject_type AND subject_id=e.subject_id AND data=e.data) THEN
    CONTINUE;
   END IF;
   RAISE EXCEPTION 'event ID already records a different fact' USING ERRCODE='23505';
  END IF;
  UPDATE "billing".eventlog_streams SET position=position+1,head_id=coalesce(e.boundary,e.id),
   recorded_at=greatest(recorded_at,clock_timestamp())
   WHERE source=e.source AND scope=e.scope RETURNING position,recorded_at INTO next_position,stamp;
  INSERT INTO "billing".eventlog_events(source,scope,position,id,boundary,event_source,event_type,version,occurred_at,recorded_at,subject_type,subject_id,data)
   VALUES(e.source,e.scope,next_position,e.id,coalesce(e.boundary,e.id),e.event_source,e.event_type,e.version,e.occurred_at,stamp,e.subject_type,e.subject_id,e.data);
 END LOOP;
 DELETE FROM "billing".eventlog_pending WHERE transaction_id=pg_current_xact_id();
 RETURN NULL;
END $fn$;

INSERT INTO "billing".eventlog_migrations(version) VALUES(2);
-- END helpers/eventlog/postgres schema v2
