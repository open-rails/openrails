-- Deployment-owned credentials; library initializers grant their own access.
-- Concurrent provisioning can race on the cluster-wide role catalog.
SELECT format($command$
DO $role$
BEGIN
    CREATE ROLE %I LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD %L;
EXCEPTION WHEN duplicate_object OR unique_violation THEN
    NULL;
END;
$role$;
$command$, :'runtime_user', :'runtime_password')
\gexec
