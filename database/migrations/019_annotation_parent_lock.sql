-- epistula-api serializes an annotation write with IMAP MOVE by holding the parent
-- message row FOR KEY SHARE until it commits (see api
-- annotation.go). PostgreSQL requires UPDATE privilege for any row-locking
-- clause, and the reader's role must never have it: epistula_api holds SELECT on
-- messages and nothing more, so every annotation write failed with
-- "permission denied for table messages" (OPS-008).
--
-- This function takes exactly that lock, with its owner's privileges, and does
-- nothing else. It returns the id when the message exists and no row when it
-- does not, so a caller distinguishes "gone" without a second query. The lock
-- is a row lock, so it is held until the CALLER's transaction ends.
--
-- SECURITY DEFINER, so the search_path is pinned and the table qualified: a
-- caller cannot substitute its own "messages" relation.
CREATE OR REPLACE FUNCTION mail_lock_message_for_annotation(p_message_id BIGINT)
RETURNS SETOF BIGINT
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
    SELECT id FROM public.messages WHERE id = p_message_id FOR KEY SHARE
$$;

-- A new function is executable by PUBLIC. Every role that can connect would
-- then be able to hold message rows locked, blocking EXPUNGE and MOVE for as
-- long as it keeps a transaction open. Revoke that, and grant the function to
-- exactly the roles that may write annotations, the only writers that need it.
-- A role created after this migration gets it from its deploy grants
-- (api/deploy/README.md).
REVOKE ALL ON FUNCTION mail_lock_message_for_annotation(BIGINT) FROM PUBLIC;

DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN
        SELECT rolname FROM pg_roles
         WHERE NOT rolsuper
           AND rolname !~ '^pg_'
           AND rolname <> current_user
           AND has_table_privilege(oid, 'public.message_annotations', 'INSERT')
    LOOP
        EXECUTE format('GRANT EXECUTE ON FUNCTION mail_lock_message_for_annotation(BIGINT) TO %I', r.rolname);
    END LOOP;
END
$$;
