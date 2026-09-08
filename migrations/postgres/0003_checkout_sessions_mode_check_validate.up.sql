-- Validate the mode CHECK added NOT VALID by 0002. VALIDATE CONSTRAINT scans
-- existing rows under SHARE UPDATE EXCLUSIVE, which does not block writes; in its
-- own transaction it never extends the lock 0002 took.

SET statement_timeout = '300s';
SET lock_timeout = '10s';

ALTER TABLE openrails.checkout_sessions
    VALIDATE CONSTRAINT checkout_sessions_mode_check;
