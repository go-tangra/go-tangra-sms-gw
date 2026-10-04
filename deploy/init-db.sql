-- Development database of the sms-gw module (deploy/compose.yaml service
-- postgres; never use these credentials outside a workstation). The
-- application role has no BYPASSRLS: row-level security backs every tenant
-- predicate. `smsgwsvc bootstrap` migrates with the postgres role and grants
-- this role its table rights.
CREATE ROLE smsgw_app LOGIN PASSWORD 'dev' NOBYPASSRLS;
GRANT CONNECT ON DATABASE sms_gw TO smsgw_app;
