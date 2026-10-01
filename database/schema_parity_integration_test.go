package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ptudor/epistula-mail/database/pgtest"
	"github.com/ptudor/epistula-mail/database/storage"
)

// TestSchemaSQLMatchesMigrations locks down the "bootstrap from either"
// guarantee: applying schema.sql to an empty database must produce exactly
// the same schema — including column ordinal positions — as running every
// migration in sequence. A divergence here means pg_dump diffs, SELECT *
// ordinal drift, and operator surprise.
func TestSchemaSQLMatchesMigrations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	migrated, _ := pgtest.Open(t)
	bare, _ := pgtest.OpenBare(t)

	schemaSQL, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("read schema.sql: %v", err)
	}
	if _, err := bare.Pool().Exec(ctx, string(schemaSQL)); err != nil {
		t.Fatalf("apply schema.sql: %v", err)
	}

	type colKey struct {
		Table   string
		Ordinal int
	}
	type colVal struct {
		Name     string
		Type     string
		Nullable string
		Default  string
	}
	dump := func(db *storage.DB) map[colKey]colVal {
		rows, err := db.Pool().Query(ctx, `
			SELECT table_name, ordinal_position, column_name, data_type,
			       is_nullable, COALESCE(column_default, '')
			  FROM information_schema.columns
			 WHERE table_schema = 'public'
			 ORDER BY table_name, ordinal_position`)
		if err != nil {
			t.Fatalf("query columns: %v", err)
		}
		defer rows.Close()
		out := map[colKey]colVal{}
		for rows.Next() {
			var k colKey
			var v colVal
			if err := rows.Scan(&k.Table, &k.Ordinal, &v.Name, &v.Type, &v.Nullable, &v.Default); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[k] = v
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	fromMigrations := dump(migrated)
	fromSchemaSQL := dump(bare)

	for k, mv := range fromMigrations {
		sv, ok := fromSchemaSQL[k]
		if !ok {
			t.Errorf("%s ordinal %d (%s via migrations) missing in schema.sql bootstrap", k.Table, k.Ordinal, mv.Name)
			continue
		}
		if mv != sv {
			t.Errorf("%s ordinal %d differs:\n  migrations: %+v\n  schema.sql: %+v", k.Table, k.Ordinal, mv, sv)
		}
	}
	for k, sv := range fromSchemaSQL {
		if _, ok := fromMigrations[k]; !ok {
			t.Errorf("%s ordinal %d (%s via schema.sql) missing in migrations bootstrap", k.Table, k.Ordinal, sv.Name)
		}
	}

	// Index parity (names + definitions, normalized by name sort).
	dumpIndexes := func(db *storage.DB) map[string]string {
		rows, err := db.Pool().Query(ctx, `
			SELECT indexname, indexdef FROM pg_indexes
			 WHERE schemaname = 'public' ORDER BY indexname`)
		if err != nil {
			t.Fatalf("query indexes: %v", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				t.Fatalf("scan index: %v", err)
			}
			out[name] = def
		}
		return out
	}
	mi := dumpIndexes(migrated)
	si := dumpIndexes(bare)
	for name, def := range mi {
		if sdef, ok := si[name]; !ok {
			t.Errorf("index %s exists via migrations but not schema.sql", name)
		} else if def != sdef {
			t.Errorf("index %s differs:\n  migrations: %s\n  schema.sql: %s", name, def, sdef)
		}
	}
	for name := range si {
		if _, ok := mi[name]; !ok {
			t.Errorf("index %s exists via schema.sql but not migrations", name)
		}
	}

	// Function parity: signature, SECURITY DEFINER, pinned settings and the
	// grants. mail_lock_message_for_annotation is only safe with all of them
	// (OPS-008), and a bootstrap from schema.sql must not leave it executable
	// by PUBLIC or unpinned.
	dumpFunctions := func(db *storage.DB) map[string]string {
		rows, err := db.Pool().Query(ctx, `
			SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
			       format('secdef=%s config=%s acl=%s', p.prosecdef,
			              COALESCE(array_to_string(p.proconfig, ';'), ''),
			              COALESCE(p.proacl::text, 'default'))
			  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = 'public'`)
		if err != nil {
			t.Fatalf("query functions: %v", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var name, attrs string
			if err := rows.Scan(&name, &attrs); err != nil {
				t.Fatalf("scan function: %v", err)
			}
			out[name] = attrs
		}
		return out
	}
	mf := dumpFunctions(migrated)
	sf := dumpFunctions(bare)
	for name, attrs := range mf {
		if sattrs, ok := sf[name]; !ok {
			t.Errorf("function %s exists via migrations but not schema.sql", name)
		} else if attrs != sattrs {
			t.Errorf("function %s differs:\n  migrations: %s\n  schema.sql: %s", name, attrs, sattrs)
		}
	}
	for name := range sf {
		if _, ok := mf[name]; !ok {
			t.Errorf("function %s exists via schema.sql but not migrations", name)
		}
	}

	// Both bootstraps must record the same migration versions so `migrate
	// status` agrees on a schema.sql-bootstrapped deployment.
	versions := func(db *storage.DB) string {
		var out string
		if err := db.Pool().QueryRow(ctx,
			`SELECT COALESCE(string_agg(version::text, ',' ORDER BY version), '') FROM schema_versions`,
		).Scan(&out); err != nil {
			t.Fatalf("schema_versions: %v", err)
		}
		return out
	}
	if mv, sv := versions(migrated), versions(bare); mv != sv {
		t.Errorf("schema_versions differ: migrations=%q schema.sql=%q", mv, sv)
	}
}
