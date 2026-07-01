-- #658: termination events (revoke/expire/supersede) are point-in-time supersession
-- markers, not access windows — only grant events carry ends_at. Backfill any
-- historical termination rows that copied the grant's ends_at (pre-v0.81.0
-- terminate() behaviour), then enforce the invariant so a windowed termination can
-- never recur. A windowed termination is exactly what let a revoke of an
-- already-expired grant (starts_at = now() > the grant's past ends_at) violate
-- grants_valid_window and break bulk reconcile of migrated/expired subscriptions.
UPDATE openrails.grants
   SET ends_at = NULL
 WHERE event <> 'grant' AND ends_at IS NOT NULL;

ALTER TABLE openrails.grants
    ADD CONSTRAINT grants_termination_no_window
    CHECK (event = 'grant' OR ends_at IS NULL);

COMMENT ON CONSTRAINT grants_termination_no_window ON openrails.grants IS
    'Only grant events carry an access window; revoke/expire/supersede are window-less point events (valid-time instant on starts_at, transaction time on created_at).';
