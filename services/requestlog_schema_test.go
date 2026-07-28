package services

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openRequestLogSchemaTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "request-log.db"))
	if err != nil {
		t.Fatalf("open sqlite database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRequestLogIndexesAreAutomaticOnlyForEmptyTables(t *testing.T) {
	t.Run("fresh empty table creates indexes", func(t *testing.T) {
		db := openRequestLogSchemaTestDB(t)
		if err := ensureRequestLogTableWithDB(db); err != nil {
			t.Fatalf("ensureRequestLogTableWithDB failed: %v", err)
		}
		if err := prepareRequestLogIndexes(db, databaseInitNormal); err != nil {
			t.Fatalf("prepareRequestLogIndexes empty table failed: %v", err)
		}
		assertAllRequestLogIndexesExist(t, db)
	})

	t.Run("non-empty legacy table fails without creating indexes", func(t *testing.T) {
		db := openRequestLogSchemaTestDB(t)
		if err := ensureRequestLogTableWithDB(db); err != nil {
			t.Fatalf("ensureRequestLogTableWithDB failed: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO request_log (user_id, created_at) VALUES (?, ?)`, "user-a", "2026-01-01 00:00:00"); err != nil {
			t.Fatalf("insert legacy request_log row: %v", err)
		}

		err := prepareRequestLogIndexes(db, databaseInitNormal)
		if !errors.Is(err, ErrRequestLogIndexMigrationRequired) {
			t.Fatalf("prepareRequestLogIndexes error = %v, want migration required", err)
		}
		if !strings.Contains(err.Error(), RequestLogIndexMigrationCommand) {
			t.Fatalf("migration-required error does not name maintenance command: %v", err)
		}
		wrapped := fmt.Errorf("wrapped startup error: %w", err)
		if !errors.Is(wrapped, ErrRequestLogIndexMigrationRequired) {
			t.Fatalf("wrapped error no longer matches migration sentinel: %v", wrapped)
		}
		missing, listErr := missingRequestLogIndexes(db)
		if listErr != nil {
			t.Fatalf("missingRequestLogIndexes failed: %v", listErr)
		}
		if len(missing) != len(requestLogIndexDefinitions) {
			t.Fatalf("normal startup created indexes on non-empty table; missing=%v", missing)
		}
	})

	t.Run("partial legacy migration is not continued during startup", func(t *testing.T) {
		db := openRequestLogSchemaTestDB(t)
		if err := ensureRequestLogTableWithDB(db); err != nil {
			t.Fatalf("ensureRequestLogTableWithDB failed: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO request_log (user_id, created_at) VALUES (?, ?)`, "user-a", "2026-01-01 00:00:00"); err != nil {
			t.Fatalf("insert legacy request_log row: %v", err)
		}
		if _, err := db.Exec(requestLogIndexDefinitions[0].createSQL); err != nil {
			t.Fatalf("create first request_log index: %v", err)
		}

		err := prepareRequestLogIndexes(db, databaseInitNormal)
		if !errors.Is(err, ErrRequestLogIndexMigrationRequired) {
			t.Fatalf("prepareRequestLogIndexes error = %v, want migration required", err)
		}
		missing, listErr := missingRequestLogIndexes(db)
		if listErr != nil {
			t.Fatalf("missingRequestLogIndexes failed: %v", listErr)
		}
		if len(missing) != len(requestLogIndexDefinitions)-1 {
			t.Fatalf("normal startup continued partial migration; missing=%v", missing)
		}
	})
}

func TestTrafficColumnsUpgradePopulatedRequestLogWithoutMaintenanceMigration(t *testing.T) {
	db := openRequestLogSchemaTestDB(t)
	if _, err := db.Exec(`CREATE TABLE request_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create legacy request_log: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO request_log (user_id) VALUES ('legacy-user')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := ensureRequestLogTableWithDB(db); err != nil {
		t.Fatalf("upgrade request_log: %v", err)
	}
	if err := ensureTrafficSchema(db); err != nil {
		t.Fatalf("ensure traffic schema: %v", err)
	}

	var userID, traceID, scope string
	var clientRequestBytes, upstreamAttempts int64
	if err := db.QueryRow(`
		SELECT user_id, COALESCE(traffic_trace_id, ''), COALESCE(client_network_scope, ''),
			client_request_bytes, upstream_attempts
		FROM request_log WHERE id = 1`).Scan(
		&userID, &traceID, &scope, &clientRequestBytes, &upstreamAttempts,
	); err != nil {
		t.Fatalf("read upgraded legacy row: %v", err)
	}
	if userID != "legacy-user" || traceID != "" || scope != "" || clientRequestBytes != 0 || upstreamAttempts != 0 {
		t.Fatalf("upgraded legacy row changed unexpectedly: user=%q trace=%q scope=%q client=%d attempts=%d",
			userID, traceID, scope, clientRequestBytes, upstreamAttempts)
	}
}

func TestExplicitRequestLogIndexMigrationIsIdempotentAndUsesExpectedPlans(t *testing.T) {
	db := openRequestLogSchemaTestDB(t)
	if err := ensureRequestLogTableWithDB(db); err != nil {
		t.Fatalf("ensureRequestLogTableWithDB failed: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO request_log (user_id, platform, http_code, created_at) VALUES (?, ?, ?, ?)`, "user-a", "claude", 500, "2026-01-01 00:00:00"); err != nil {
		t.Fatalf("insert request_log row: %v", err)
	}
	// Simulate an interrupted/partially completed maintenance run.
	if _, err := db.Exec(requestLogIndexDefinitions[0].createSQL); err != nil {
		t.Fatalf("create first request_log index: %v", err)
	}

	for run := 1; run <= 2; run++ {
		if err := prepareRequestLogIndexes(db, databaseInitMigrateRequestLogIndexes); err != nil {
			t.Fatalf("explicit migration run %d failed: %v", run, err)
		}
	}
	assertAllRequestLogIndexesExist(t, db)

	assertQueryPlanUsesIndex(t, db, "idx_request_log_created_at",
		`SELECT COUNT(*) FROM request_log WHERE created_at >= ? AND created_at < ?`, "2026-01-01", "2026-01-02")
	assertQueryPlanUsesIndex(t, db, "idx_request_log_user_platform_created_at",
		`SELECT COUNT(*) FROM request_log WHERE user_id = ? AND platform = ? AND created_at >= ? AND created_at < ?`, "user-a", "claude", "2026-01-01", "2026-01-02")
	assertQueryPlanUsesIndex(t, db, "idx_request_log_user_created_at",
		`SELECT COUNT(*) FROM request_log WHERE user_id = ? AND created_at >= ? AND created_at < ?`, "user-a", "2026-01-01", "2026-01-02")
	assertQueryPlanUsesIndex(t, db, "idx_request_log_user_id",
		`SELECT id FROM request_log WHERE user_id = ? ORDER BY id DESC LIMIT 100`, "user-a")
	assertQueryPlanUsesIndex(t, db, "idx_request_log_user_error_id",
		`SELECT id FROM request_log WHERE user_id = ? AND http_code >= 400 ORDER BY id DESC LIMIT 100`, "user-a")
}

func assertAllRequestLogIndexesExist(t *testing.T, db *sql.DB) {
	t.Helper()
	missing, err := missingRequestLogIndexes(db)
	if err != nil {
		t.Fatalf("missingRequestLogIndexes failed: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing request_log indexes: %v", missing)
	}
}

func assertQueryPlanUsesIndex(t *testing.T, db *sql.DB, indexName string, query string, args ...any) {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain query plan for %s: %v", indexName, err)
	}
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan query plan for %s: %v", indexName, err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query plan for %s: %v", indexName, err)
	}
	if !strings.Contains(strings.Join(details, "\n"), indexName) {
		t.Fatalf("query plan does not use %s: %s", indexName, strings.Join(details, "; "))
	}
}
