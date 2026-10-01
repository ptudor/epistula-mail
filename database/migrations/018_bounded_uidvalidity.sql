-- All folder creation/rename paths use this allocator. Never wrap or reset the
-- shared sequence: exhaustion requires operator recovery with new identities.
CREATE OR REPLACE FUNCTION mail_next_uidvalidity() RETURNS BIGINT AS $$
DECLARE value BIGINT;
BEGIN
    value := nextval('folder_uidvalidity_seq');
    IF value < 1 OR value > 4294967295 THEN
        RAISE EXCEPTION 'IMAP UIDVALIDITY exceeds the 32-bit protocol range'
            USING ERRCODE = '22003';
    END IF;
    RETURN value;
END;
$$ LANGUAGE plpgsql;
