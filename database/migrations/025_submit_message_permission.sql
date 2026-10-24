-- Outbound submission through epistula-api: the submit_message permission.
--
-- epistula-api can hand a complete RFC 5322 message, byte for byte, to the
-- MTA's authenticated submission service (api/CLAUDE.md, "Message
-- submission"). Sending mail is the one capability in this stack whose effect
-- leaves the host, so it is a permission of its own rather than an extension
-- of write_annotation: a token that may annotate or classify must never
-- thereby be able to send, and epistula-mcp's interactive connector holds it
-- only on a separate credential it registers no tool for by default.
--
-- No table changes and no grants: epistula-api checks the envelope sender
-- against aliases and domains, which its role can already SELECT.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_permissions_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_permissions_check CHECK (
    cardinality(permissions) > 0 AND
    permissions <@ ARRAY['read_metadata', 'read_content', 'write_annotation', 'write_classification', 'submit_message']
);
