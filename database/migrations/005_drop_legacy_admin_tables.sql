-- Remove speculative Go-admin web auth tables.
--
-- Management is the `epistula-database admin` CLI. A web operator dashboard
-- is not included; external administration tools own their own auth/session
-- tables, so the mail store schema does not carry unused admin cookies or
-- CSRF state.

DROP TABLE IF EXISTS admin_sessions;
DROP TABLE IF EXISTS admin_users;
