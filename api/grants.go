package main

import (
	"context"
	"log/slog"
)

// probeWriteGrants gates sidecars on every privilege needed by their reads,
// inserts and upserts, including the shared parent-row locking function.
func (s *server) probeWriteGrants(ctx context.Context) (annotations, classifications bool, err error) {
	annotations, classifications = true, true
	var missing []string
	for _, need := range []struct{ table, privilege string }{
		{"message_annotations", "SELECT"},
		{"message_annotations", "INSERT"},
		{"message_annotations", "UPDATE"},
		{"message_classifications", "SELECT"},
		{"message_classifications", "INSERT"},
		{"message_classifications", "UPDATE"},
		{"archive_categories", "SELECT"},
	} {
		var granted bool
		if err := s.pool.QueryRow(ctx, `SELECT CASE WHEN to_regclass($1) IS NULL THEN false
			ELSE has_table_privilege($1, $2) END`, need.table, need.privilege).Scan(&granted); err != nil {
			return false, false, err
		}
		if !granted {
			missing = append(missing, need.privilege+" ON "+need.table)
			if need.table == "message_annotations" {
				annotations = false
			} else {
				classifications = false
			}
		}
	}
	const lockFunction = "mail_lock_message_for_annotation(bigint)"
	var executable bool
	if err := s.pool.QueryRow(ctx, `SELECT CASE WHEN to_regprocedure($1) IS NULL THEN false
		ELSE has_function_privilege($1, 'EXECUTE') END`, lockFunction).Scan(&executable); err != nil {
		return false, false, err
	}
	if !executable {
		missing = append(missing, "EXECUTE ON FUNCTION "+lockFunction)
		annotations, classifications = false, false
	}
	if len(missing) != 0 {
		slog.Warn("sidecar grants missing or objects absent; affected endpoints will return 503", "missing_grants", missing)
	}
	return annotations, classifications, nil
}
