package services

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteDatabaseDSNAppliesBusyTimeoutToEveryPhysicalConnection(t *testing.T) {
	dsn := sqliteDatabaseDSN(filepath.Join(t.TempDir(), "busy-timeout.db"))
	queryStart := strings.IndexByte(dsn, '?')
	if queryStart < 0 {
		t.Fatalf("sqlite DSN has no query: %q", dsn)
	}
	query, err := url.ParseQuery(dsn[queryStart+1:])
	if err != nil {
		t.Fatalf("parse sqlite DSN query: %v", err)
	}
	if got := query.Get("_pragma"); got != "busy_timeout=30000" {
		t.Fatalf("sqlite DSN _pragma = %q, want busy_timeout=30000", got)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(sqliteMaxOpenConnections)
	db.SetMaxIdleConns(sqliteMaxIdleConnections)

	first := acquireConnectionsAndAssertBusyTimeout(t, db, sqliteMaxOpenConnections)
	if got := db.Stats().OpenConnections; got != sqliteMaxOpenConnections {
		t.Fatalf("first open connections = %d, want %d", got, sqliteMaxOpenConnections)
	}

	// Force every current physical connection out of the pool, then prove newly
	// created replacements receive the same DSN pragma.
	db.SetMaxIdleConns(0)
	closeSQLConnections(t, first)
	if got := db.Stats().OpenConnections; got != 0 {
		t.Fatalf("open connections after idle eviction = %d, want 0", got)
	}
	db.SetMaxIdleConns(sqliteMaxIdleConnections)

	second := acquireConnectionsAndAssertBusyTimeout(t, db, sqliteMaxOpenConnections)
	closeSQLConnections(t, second)
}

func TestSQLiteMigrationDatabaseDSNRequiresExistingFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing app.db")
	dsn := sqliteMigrationDatabaseDSN(dbPath)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse migration sqlite DSN: %v", err)
	}
	if parsed.Scheme != "file" || filepath.Clean(parsed.Path) != dbPath {
		t.Fatalf("migration sqlite DSN = %q, want file URI for %q", dsn, dbPath)
	}
	query := parsed.Query()
	if got := query.Get("mode"); got != "rw" {
		t.Fatalf("migration sqlite DSN mode = %q, want rw", got)
	}
	if got := query.Get("_pragma"); got != "busy_timeout=30000" {
		t.Fatalf("migration sqlite DSN _pragma = %q, want busy_timeout=30000", got)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open migration database: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err == nil {
		t.Fatal("mode=rw unexpectedly opened a missing database")
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mode=rw created missing database %q: stat error = %v", dbPath, err)
	}
}

func TestMigrateRequestLogIndexesWrongHomeDoesNotCreateDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	err := MigrateRequestLogIndexes()
	if err == nil || !strings.Contains(err.Error(), "迁移目标数据库不存在") {
		t.Fatalf("MigrateRequestLogIndexes error = %v, want missing target", err)
	}
	configDir := filepath.Join(home, ".code-switch")
	if _, err := os.Stat(configDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("migration created config directory %q: stat error = %v", configDir, err)
	}
}

func TestRequestLogIndexMigrationValidatesTargetBeforeWrites(t *testing.T) {
	t.Run("database without request_log", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), ".code-switch", "app.db")
		createMigrationTargetDatabase(t, dbPath, func(db *sql.DB) {
			if _, err := db.Exec(`CREATE TABLE unrelated (value TEXT)`); err != nil {
				t.Fatalf("create unrelated table: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO unrelated (value) VALUES ('sentinel')`); err != nil {
				t.Fatalf("insert unrelated sentinel: %v", err)
			}
		})
		info, err := os.Stat(dbPath)
		if err != nil {
			t.Fatalf("stat migration target: %v", err)
		}

		var output bytes.Buffer
		err = migrateRequestLogIndexesAtPath(dbPath, &output)
		if err == nil || !strings.Contains(err.Error(), "缺少 request_log 表") {
			t.Fatalf("migration error = %v, want missing request_log", err)
		}
		assertMigrationTargetOutput(t, output.String(), dbPath, info.Size())

		db := openExistingMigrationTargetDatabase(t, dbPath)
		defer db.Close()
		assertSchemaObjectAbsent(t, db, "request_log")
		assertSchemaObjectAbsent(t, db, "app_settings")
		assertJournalMode(t, db, "delete")
		var value string
		if err := db.QueryRow(`SELECT value FROM unrelated LIMIT 1`).Scan(&value); err != nil || value != "sentinel" {
			t.Fatalf("unrelated sentinel = %q, err = %v", value, err)
		}
	})

	t.Run("empty request_log", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), ".code-switch", "app.db")
		createMigrationTargetDatabase(t, dbPath, func(db *sql.DB) {
			if _, err := db.Exec(`CREATE TABLE request_log (id INTEGER PRIMARY KEY)`); err != nil {
				t.Fatalf("create empty request_log: %v", err)
			}
		})

		err := migrateRequestLogIndexesAtPath(dbPath, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "request_log 表为空") {
			t.Fatalf("migration error = %v, want empty request_log", err)
		}

		db := openExistingMigrationTargetDatabase(t, dbPath)
		defer db.Close()
		missing, listErr := missingRequestLogIndexes(db)
		if listErr != nil {
			t.Fatalf("list request_log indexes: %v", listErr)
		}
		if len(missing) != len(requestLogIndexDefinitions) {
			t.Fatalf("empty target gained indexes: missing = %v", missing)
		}
		assertSchemaObjectAbsent(t, db, "app_settings")
		assertJournalMode(t, db, "delete")
	})

	t.Run("non-empty request_log migrates idempotently", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), ".code-switch", "app.db")
		createMigrationTargetDatabase(t, dbPath, func(db *sql.DB) {
			if err := ensureRequestLogTableWithDB(db); err != nil {
				t.Fatalf("create request_log: %v", err)
			}
			if _, err := db.Exec(
				`INSERT INTO request_log (user_id, platform, http_code, created_at) VALUES (?, ?, ?, ?)`,
				"migration-sentinel", "claude", 200, "2026-01-01 00:00:00",
			); err != nil {
				t.Fatalf("insert request_log sentinel: %v", err)
			}
			if _, err := db.Exec(requestLogIndexDefinitions[0].createSQL); err != nil {
				t.Fatalf("create partial migration index: %v", err)
			}
		})

		for run := 1; run <= 2; run++ {
			if err := migrateRequestLogIndexesAtPath(dbPath, &bytes.Buffer{}); err != nil {
				t.Fatalf("migration run %d failed: %v", run, err)
			}
		}

		db := openExistingMigrationTargetDatabase(t, dbPath)
		defer db.Close()
		assertAllRequestLogIndexesExist(t, db)
		assertSchemaObjectAbsent(t, db, "app_settings")
		assertJournalMode(t, db, "delete")
		var userID string
		if err := db.QueryRow(`SELECT user_id FROM request_log LIMIT 1`).Scan(&userID); err != nil || userID != "migration-sentinel" {
			t.Fatalf("request_log sentinel = %q, err = %v", userID, err)
		}
	})
}

func createMigrationTargetDatabase(t *testing.T, dbPath string, setup func(*sql.DB)) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("create migration target directory: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open migration target setup database: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping migration target setup database: %v", err)
	}
	setup(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close migration target setup database: %v", err)
	}
}

func openExistingMigrationTargetDatabase(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteMigrationDatabaseDSN(dbPath))
	if err != nil {
		t.Fatalf("open existing migration target: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping existing migration target: %v", err)
	}
	return db
}

func assertMigrationTargetOutput(t *testing.T, output string, dbPath string, size int64) {
	t.Helper()
	want := fmt.Sprintf("database=%q size_bytes=%d", dbPath, size)
	if !strings.Contains(output, want) {
		t.Fatalf("migration output = %q, want %q", output, want)
	}
}

func assertSchemaObjectAbsent(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var ready int
	err := db.QueryRow(`SELECT 1 FROM sqlite_schema WHERE name = ? LIMIT 1`, name).Scan(&ready)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("schema object %q unexpectedly exists or query failed: %v", name, err)
	}
}

func assertJournalMode(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&got); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if got != want {
		t.Fatalf("journal_mode = %q, want %q", got, want)
	}
}

func acquireConnectionsAndAssertBusyTimeout(t *testing.T, db *sql.DB, count int) []*sql.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connections := make([]*sql.Conn, 0, count)
	for index := 0; index < count; index++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			closeSQLConnections(t, connections)
			t.Fatalf("acquire sqlite connection %d: %v", index, err)
		}
		connections = append(connections, conn)

		var busyTimeout int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			closeSQLConnections(t, connections)
			t.Fatalf("query busy_timeout on connection %d: %v", index, err)
		}
		if busyTimeout != sqliteBusyTimeoutMilliseconds {
			closeSQLConnections(t, connections)
			t.Fatalf("connection %d busy_timeout = %d, want %d", index, busyTimeout, sqliteBusyTimeoutMilliseconds)
		}
	}
	return connections
}

func closeSQLConnections(t *testing.T, connections []*sql.Conn) {
	t.Helper()
	for index, conn := range connections {
		if err := conn.Close(); err != nil {
			t.Errorf("close sqlite connection %d: %v", index, err)
		}
	}
}
