package services

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
)

type requestLogRollupTotals struct {
	TotalRequests     int64
	InputTokens       int64
	OutputTokens      int64
	ReasoningTokens   int64
	CacheCreateTokens int64
	CacheReadTokens   int64
}

type requestLogRollupRow struct {
	Bucket   string
	UserID   string
	Platform string
	Totals   requestLogRollupTotals
}

type requestLogRollupExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func TestRequestLogUsageRollupTriggerBucketsIsolationAndRollback(t *testing.T) {
	db := openRequestLogUsageRollupTestDB(t, true)
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin rollback transaction: %v", err)
	}
	insertRequestLogUsageRollupRow(t, tx, "rollback-user", "claude", "2026-07-23 16:00:00", 9, 8, 7, 6, 5, false)
	if got := readRequestLogUsageRollupTotals(t, tx, "2026-07-23 16:00:00", "rollback-user", "claude"); got.TotalRequests != 1 {
		t.Fatalf("rollup inside transaction = %+v, want one request", got)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback request_log insert: %v", err)
	}
	assertRequestLogUsageRollupRowCount(t, db, "rollback-user", "claude", 0)

	// 15:59 UTC is 23:59 Beijing and belongs to the UTC 15:30 bucket.
	insertRequestLogUsageRollupRow(t, db, "boundary-user", "claude", "2026-07-23 15:59:59", 1, 2, 3, 4, 5, false)
	assertRequestLogUsageRollupTotals(t, db, "2026-07-23 15:30:00", "boundary-user", "claude", requestLogRollupTotals{
		TotalRequests: 1, InputTokens: 1, OutputTokens: 2, ReasoningTokens: 3, CacheCreateTokens: 4, CacheReadTokens: 5,
	})

	// 16:00 UTC is 00:00 Beijing on the next day. 16:29 stays in that
	// bucket, while 16:30 starts the next half-hour bucket.
	insertRequestLogUsageRollupRow(t, db, "user-a", "claude", "2026-07-23 16:00:00", 10, 20, 30, 40, 50, false)
	insertRequestLogUsageRollupRow(t, db, "user-a", "claude", "2026-07-23 16:29:59", 100, 200, 300, 400, 500, true)
	insertRequestLogUsageRollupRow(t, db, "user-a", "claude", "2026-07-23 16:30:00", 11, 21, 31, 41, 51, false)
	insertRequestLogUsageRollupRow(t, db, "user-b", "claude", "2026-07-23 16:00:00", 7, 8, 9, 10, 11, false)
	insertRequestLogUsageRollupRow(t, db, "user-a", "openai-chat", "2026-07-23 16:00:00", 13, 14, 15, 16, 17, false)

	assertRequestLogUsageRollupTotals(t, db, "2026-07-23 16:00:00", "user-a", "claude", requestLogRollupTotals{
		TotalRequests:     2,
		InputTokens:       10,
		OutputTokens:      20,
		ReasoningTokens:   30,
		CacheCreateTokens: 40,
		CacheReadTokens:   50,
	})
	assertRequestLogUsageRollupTotals(t, db, "2026-07-23 16:30:00", "user-a", "claude", requestLogRollupTotals{
		TotalRequests: 1, InputTokens: 11, OutputTokens: 21, ReasoningTokens: 31, CacheCreateTokens: 41, CacheReadTokens: 51,
	})
	assertRequestLogUsageRollupTotals(t, db, "2026-07-23 16:00:00", "user-b", "claude", requestLogRollupTotals{
		TotalRequests: 1, InputTokens: 7, OutputTokens: 8, ReasoningTokens: 9, CacheCreateTokens: 10, CacheReadTokens: 11,
	})
	assertRequestLogUsageRollupTotals(t, db, "2026-07-23 16:00:00", "user-a", "openai-chat", requestLogRollupTotals{
		TotalRequests: 1, InputTokens: 13, OutputTokens: 14, ReasoningTokens: 15, CacheCreateTokens: 16, CacheReadTokens: 17,
	})
}

func TestRequestLogUsageRollupBackfillIsIdempotentAndMarksReady(t *testing.T) {
	db := openRequestLogUsageRollupTestDB(t, false)
	defer db.Close()

	now := time.Date(2026, time.July, 24, 12, 0, 0, 0, beijingLocation)
	dayStart := startOfDay(now)
	insertRequestLogUsageRollupRow(t, db, "user-a", "claude", dayStart.Add(10*time.Minute).UTC().Format(timeLayout), 3, 4, 5, 6, 7, false)
	insertRequestLogUsageRollupRow(t, db, "user-b", "openai-chat", dayStart.Add(35*time.Minute).UTC().Format(timeLayout), 8, 9, 10, 11, 12, true)

	if err := ensureRequestLogRollupSchema(db); err != nil {
		t.Fatalf("ensure request_log rollup schema: %v", err)
	}
	if err := requireRequestLogRollupReady(db); !errors.Is(err, ErrRequestLogRollupMigrationRequired) {
		t.Fatalf("unmigrated non-empty database readiness error = %v, want ErrRequestLogRollupMigrationRequired", err)
	}

	// This row arrives after the trigger exists but before the rebuild. The
	// rebuild must replace, rather than add to, the live partial aggregate.
	insertRequestLogUsageRollupRow(t, db, "user-a", "claude", dayStart.Add(40*time.Minute).UTC().Format(timeLayout), 13, 14, 15, 16, 17, false)

	var firstOutput bytes.Buffer
	if err := rebuildCurrentBeijingDayRequestLogRollup(db, now, &firstOutput); err != nil {
		t.Fatalf("first request_log rollup rebuild: %v", err)
	}
	first := readAllRequestLogUsageRollupRows(t, db)
	if len(first) != 3 {
		t.Fatalf("first rebuild rows = %+v, want three isolated buckets", first)
	}
	if !strings.Contains(firstOutput.String(), "buckets=48") {
		t.Fatalf("first rebuild output = %q, want buckets=48", firstOutput.String())
	}

	var secondOutput bytes.Buffer
	if err := rebuildCurrentBeijingDayRequestLogRollup(db, now, &secondOutput); err != nil {
		t.Fatalf("second request_log rollup rebuild: %v", err)
	}
	second := readAllRequestLogUsageRollupRows(t, db)
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("second rebuild changed rollup:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	if err := requireRequestLogRollupReady(db); err != nil {
		t.Fatalf("rebuilt request_log rollup is not ready: %v", err)
	}
	var ready int
	if err := db.QueryRow(`SELECT ready FROM request_log_rollup_30m_state WHERE id = ?`, requestLogRollupStateID).Scan(&ready); err != nil {
		t.Fatalf("read request_log rollup ready marker: %v", err)
	}
	if ready != 1 {
		t.Fatalf("request_log rollup ready = %d, want 1", ready)
	}
}

func TestRequestLogUsageRollupReadinessFailClosedAndFreshReady(t *testing.T) {
	t.Run("fresh database is ready", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		if err := InitDatabase(); err != nil {
			t.Fatalf("InitDatabase on fresh database: %v", err)
		}
		db, err := xdb.DB("default")
		if err != nil {
			t.Fatalf("get fresh database: %v", err)
		}
		if err := requireRequestLogRollupReady(db); err != nil {
			t.Fatalf("fresh database rollup is not ready: %v", err)
		}
	})

	t.Run("legacy non-empty database requires maintenance", func(t *testing.T) {
		home := t.TempDir()
		dbPath := filepath.Join(home, ".code-switch", "app.db")
		if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
			t.Fatalf("create legacy config directory: %v", err)
		}
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open legacy database: %v", err)
		}
		if err := ensureRequestLogTableWithDB(db); err != nil {
			_ = db.Close()
			t.Fatalf("create legacy request_log: %v", err)
		}
		insertRequestLogUsageRollupRow(t, db, "legacy-user", "claude", time.Now().UTC().Format(timeLayout), 1, 2, 3, 4, 5, false)
		if err := db.Close(); err != nil {
			t.Fatalf("close legacy database: %v", err)
		}

		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		err = InitDatabase()
		if !errors.Is(err, ErrRequestLogIndexMigrationRequired) {
			t.Fatalf("InitDatabase legacy error = %v, want ErrRequestLogIndexMigrationRequired", err)
		}
		if !strings.Contains(strings.ToLower(err.Error()), "usage rollup") {
			t.Fatalf("InitDatabase legacy error = %v, want usage rollup maintenance guidance", err)
		}
	})
}

func TestStatsSinceUsesReadyHalfHourRollup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := InitDatabase(); err != nil {
		t.Fatalf("InitDatabase: %v", err)
	}
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("get database: %v", err)
	}

	now := time.Now().In(beijingLocation)
	dayStart := startOfDay(now)
	insertRequestLogUsageRollupRow(t, db, "stats-user", "claude", dayStart.Add(29*time.Minute+59*time.Second).UTC().Format(timeLayout), 10, 20, 30, 40, 50, false)
	insertRequestLogUsageRollupRow(t, db, "stats-user", "claude", dayStart.Add(30*time.Minute).UTC().Format(timeLayout), 100, 200, 300, 400, 500, true)
	insertRequestLogUsageRollupRow(t, db, "stats-user", "openai-chat", dayStart.Add(30*time.Minute).UTC().Format(timeLayout), 7, 8, 9, 10, 11, false)
	insertRequestLogUsageRollupRow(t, db, "other-user", "claude", dayStart.Add(30*time.Minute).UTC().Format(timeLayout), 13, 14, 15, 16, 17, false)

	stats, err := NewLogService().StatsSinceForUser("stats-user", "claude")
	if err != nil {
		t.Fatalf("StatsSinceForUser: %v", err)
	}
	if len(stats.Series) != 48 {
		t.Fatalf("StatsSince series length = %d, want 48", len(stats.Series))
	}
	if stats.TotalRequests != 2 || stats.InputTokens != 10 || stats.OutputTokens != 20 || stats.ReasoningTokens != 30 || stats.CacheCreateTokens != 40 || stats.CacheReadTokens != 50 {
		t.Fatalf("StatsSince totals = %+v, want isolated requests with excluded tokens suppressed", stats)
	}
	if stats.Series[0].Day != dayStart.Format(timeLayout) || stats.Series[0].TotalRequests != 1 || stats.Series[0].InputTokens != 10 {
		t.Fatalf("first half-hour bucket = %+v", stats.Series[0])
	}
	if stats.Series[1].Day != dayStart.Add(30*time.Minute).Format(timeLayout) || stats.Series[1].TotalRequests != 1 || stats.Series[1].InputTokens != 0 {
		t.Fatalf("second half-hour bucket = %+v", stats.Series[1])
	}

	assertStatsSinceQueriesOnlyRequestLogRollup(t)
}

func openRequestLogUsageRollupTestDB(t *testing.T, withRollup bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "request-log-rollup.db"))
	if err != nil {
		t.Fatalf("open request_log rollup database: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := ensureRequestLogTableWithDB(db); err != nil {
		_ = db.Close()
		t.Fatalf("ensure request_log table: %v", err)
	}
	if withRollup {
		if err := ensureRequestLogRollupSchema(db); err != nil {
			_ = db.Close()
			t.Fatalf("ensure request_log rollup schema: %v", err)
		}
	}
	return db
}

func insertRequestLogUsageRollupRow(
	t *testing.T,
	exec requestLogRollupExecer,
	userID string,
	platform string,
	createdAt string,
	inputTokens int,
	outputTokens int,
	reasoningTokens int,
	cacheCreateTokens int,
	cacheReadTokens int,
	exclude bool,
) {
	t.Helper()
	if _, err := exec.Exec(`
		INSERT INTO request_log (
			user_id, platform, input_tokens, output_tokens, reasoning_tokens,
			cache_create_tokens, cache_read_tokens, exclude_from_total, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, userID, platform, inputTokens, outputTokens, reasoningTokens, cacheCreateTokens, cacheReadTokens, boolToInt(exclude), createdAt); err != nil {
		t.Fatalf("insert request_log rollup fixture: %v", err)
	}
}

func readRequestLogUsageRollupTotals(t *testing.T, queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}, bucket string, userID string, platform string) requestLogRollupTotals {
	t.Helper()
	var totals requestLogRollupTotals
	if err := queryer.QueryRow(`
		SELECT total_requests, input_tokens, output_tokens, reasoning_tokens, cache_create_tokens, cache_read_tokens
		FROM request_log_rollup_30m
		WHERE bucket_start_utc = ? AND user_id = ? AND platform = ?
	`, bucket, userID, platform).Scan(
		&totals.TotalRequests,
		&totals.InputTokens,
		&totals.OutputTokens,
		&totals.ReasoningTokens,
		&totals.CacheCreateTokens,
		&totals.CacheReadTokens,
	); err != nil {
		t.Fatalf("read request_log rollup bucket %s/%s/%s: %v", bucket, userID, platform, err)
	}
	return totals
}

func assertRequestLogUsageRollupTotals(t *testing.T, db *sql.DB, bucket string, userID string, platform string, want requestLogRollupTotals) {
	t.Helper()
	if got := readRequestLogUsageRollupTotals(t, db, bucket, userID, platform); got != want {
		t.Fatalf("request_log rollup %s/%s/%s = %+v, want %+v", bucket, userID, platform, got, want)
	}
}

func assertRequestLogUsageRollupRowCount(t *testing.T, db *sql.DB, userID string, platform string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_log_rollup_30m WHERE user_id = ? AND platform = ?`, userID, platform).Scan(&got); err != nil {
		t.Fatalf("count request_log rollup rows: %v", err)
	}
	if got != want {
		t.Fatalf("request_log rollup row count for %s/%s = %d, want %d", userID, platform, got, want)
	}
}

func readAllRequestLogUsageRollupRows(t *testing.T, db *sql.DB) []requestLogRollupRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT bucket_start_utc, user_id, platform,
			total_requests, input_tokens, output_tokens, reasoning_tokens, cache_create_tokens, cache_read_tokens
		FROM request_log_rollup_30m
		ORDER BY bucket_start_utc, user_id, platform
	`)
	if err != nil {
		t.Fatalf("list request_log rollup rows: %v", err)
	}
	defer rows.Close()

	result := []requestLogRollupRow{}
	for rows.Next() {
		var row requestLogRollupRow
		if err := rows.Scan(
			&row.Bucket,
			&row.UserID,
			&row.Platform,
			&row.Totals.TotalRequests,
			&row.Totals.InputTokens,
			&row.Totals.OutputTokens,
			&row.Totals.ReasoningTokens,
			&row.Totals.CacheCreateTokens,
			&row.Totals.CacheReadTokens,
		); err != nil {
			t.Fatalf("scan request_log rollup row: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate request_log rollup rows: %v", err)
	}
	return result
}

func assertStatsSinceQueriesOnlyRequestLogRollup(t *testing.T) {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve request_log rollup test source path")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(testFile), "logservice.go"))
	if err != nil {
		t.Fatalf("read logservice.go: %v", err)
	}
	start := bytes.Index(source, []byte("func (ls *LogService) StatsSinceForUser"))
	if start < 0 {
		t.Fatal("StatsSinceForUser source not found")
	}
	rest := source[start+1:]
	endOffset := bytes.Index(rest, []byte("\nfunc "))
	if endOffset < 0 {
		t.Fatal("end of StatsSinceForUser source not found")
	}
	body := string(source[start : start+1+endOffset])
	if !strings.Contains(body, "FROM request_log_rollup_30m") {
		t.Fatal("StatsSinceForUser does not query request_log_rollup_30m")
	}
	detailQuery := regexp.MustCompile(`(?i)\bFROM\s+request_log(?:\s|$)`)
	if detailQuery.MatchString(body) {
		t.Fatal("StatsSinceForUser still queries request_log detail rows")
	}
}
