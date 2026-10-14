-- Support the audit log's ON DELETE SET NULL foreign keys without table scans.
-- Migrations run transactionally, so CREATE INDEX CONCURRENTLY cannot be used
-- here. On large stores, operators may pre-create these indexes CONCURRENTLY
-- under the same names; IF NOT EXISTS then leaves those indexes in place.
CREATE INDEX IF NOT EXISTS idx_delivery_log_message ON delivery_log (message_id) WHERE message_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_delivery_log_mailbox ON delivery_log (matched_mailbox_id) WHERE matched_mailbox_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_delivery_log_alias ON delivery_log (matched_alias_id) WHERE matched_alias_id IS NOT NULL;
