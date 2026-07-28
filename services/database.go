package services

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daodao97/xgo/xdb"
	_ "modernc.org/sqlite"
)

const (
	sqliteMaxOpenConnections        = 8
	sqliteMaxIdleConnections        = 2
	sqliteBusyTimeoutMilliseconds   = 30000
	RequestLogIndexMigrationCommand = "migrate-request-log-indexes"
	requestLogRollupStateID         = 1
)

var ErrRequestLogIndexMigrationRequired = errors.New("request_log index migration required")
var ErrRequestLogRollupMigrationRequired = errors.New("request_log 30-minute rollup migration required")

type databaseInitMode int

const (
	databaseInitNormal databaseInitMode = iota
	databaseInitMigrateRequestLogIndexes
)

// InitDatabase 初始化数据库连接（必须在所有服务构造之前调用）
// 【修复】解决数据库初始化时序问题：
// 1. 确保配置目录存在
// 2. 初始化 xdb 连接池
// 3. 显式设置 PRAGMA（WAL 模式 + busy_timeout）
// 4. 确保表结构存在
// 5. 预热连接池
func InitDatabase() error {
	return initDatabase()
}

// MigrateRequestLogIndexes explicitly creates missing request_log indexes and
// rebuilds the current Beijing day's usage rollup. Production callers must stop
// the service and run this command in a maintenance window because both steps
// can scan request_log data.
func MigrateRequestLogIndexes() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("获取用户目录失败: %w", err)
	}
	dbPath, err := appDatabasePath(home)
	if err != nil {
		return err
	}
	return migrateRequestLogIndexesAtPath(dbPath, os.Stdout)
}

func initDatabase() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("获取用户目录失败: %w", err)
	}
	dbPath, err := appDatabasePath(home)
	if err != nil {
		return err
	}

	// 1. 确保配置目录存在（SQLite 不会自动创建父目录）
	configDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	// 2. 初始化 xdb 连接池。busy_timeout 必须通过 DSN 应用到每一条物理连接。
	if err := xdb.Inits([]xdb.Config{
		{
			Name:        "default",
			Driver:      "sqlite",
			DSN:         sqliteDatabaseDSN(dbPath),
			MaxOpenConn: sqliteMaxOpenConnections,
			MaxIdleConn: sqliteMaxIdleConnections,
		},
	}); err != nil {
		return fmt.Errorf("初始化数据库失败: %w", err)
	}

	// 3. 显式设置数据库级 PRAGMA，并验证连接级 busy_timeout。
	db, err := xdb.DB("default")
	if err != nil {
		return fmt.Errorf("获取数据库连接失败: %w", err)
	}

	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		return fmt.Errorf("检查 busy_timeout 失败: %w", err)
	}
	if busyTimeout != sqliteBusyTimeoutMilliseconds {
		return fmt.Errorf("busy_timeout = %dms, want %dms", busyTimeout, sqliteBusyTimeoutMilliseconds)
	}

	// WAL 是数据库级持久设置，只需在初始化连接上确认。
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("设置 WAL 模式失败: %w", err)
	}
	fmt.Printf("✅ SQLite PRAGMA 已设置: journal_mode=%s, busy_timeout=%dms\n", journalMode, busyTimeout)

	// 4. 确保表结构存在。非空旧库缺索引时，正常启动只提示显式迁移，
	// 不在监听前隐式扫描历史表。
	if err := ensureRequestLogTable(); err != nil {
		return fmt.Errorf("初始化 request_log 表失败: %w", err)
	}
	if err := ensureTrafficSchema(db); err != nil {
		return fmt.Errorf("初始化流量统计表失败: %w", err)
	}
	if err := ensureAppSettingsTable(); err != nil {
		return fmt.Errorf("初始化 app_settings 表失败: %w", err)
	}
	if err := ensureRequestLogRollupSchema(db); err != nil {
		return fmt.Errorf("初始化 request_log 30 分钟汇总失败: %w", err)
	}
	if err := requireRequestLogRollupReady(db); err != nil {
		if errors.Is(err, ErrRequestLogRollupMigrationRequired) {
			return fmt.Errorf(
				"%w: request_log usage rollup is not ready; stop the service and run ./codeswitch-web %s as the service user",
				ErrRequestLogIndexMigrationRequired,
				RequestLogIndexMigrationCommand,
			)
		}
		return fmt.Errorf("检查 request_log 30 分钟汇总状态失败: %w", err)
	}
	hardenDatabaseFilePermissions(configDir)
	if err := prepareRequestLogIndexes(db, databaseInitNormal); err != nil {
		return fmt.Errorf("初始化 request_log 索引失败: %w", err)
	}

	// 5. 预热连接池：强制建立数据库连接，避免首次写入时失败
	var ready int
	if err := db.QueryRow("SELECT 1 FROM request_log LIMIT 1").Scan(&ready); err != nil && !errors.Is(err, sql.ErrNoRows) {
		fmt.Printf("⚠️  连接池预热查询失败: %v\n", err)
	} else {
		fmt.Printf("✅ 数据库连接已预热（request_log 可访问）\n")
	}

	return nil
}

func appDatabasePath(home string) (string, error) {
	dbPath, err := filepath.Abs(filepath.Join(home, ".code-switch", "app.db"))
	if err != nil {
		return "", fmt.Errorf("解析数据库绝对路径失败: %w", err)
	}
	return dbPath, nil
}

func migrateRequestLogIndexesAtPath(dbPath string, output io.Writer) error {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return fmt.Errorf("解析迁移数据库绝对路径失败: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("迁移目标数据库不存在: %q；请确认使用 systemd 服务用户且 HOME 正确", absPath)
		}
		return fmt.Errorf("检查迁移目标数据库失败 %q: %w", absPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("迁移目标不是普通文件: %q", absPath)
	}
	if _, err := fmt.Fprintf(output, "request_log 索引迁移目标: database=%q size_bytes=%d\n", absPath, info.Size()); err != nil {
		return fmt.Errorf("输出迁移目标失败: %w", err)
	}

	// The maintenance path must never create a database. A real file URI is
	// required because modernc/sqlite strips mode from non-file DSNs.
	db, err := sql.Open("sqlite", sqliteMigrationDatabaseDSN(absPath))
	if err != nil {
		return fmt.Errorf("打开迁移目标数据库失败: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Prove this is an existing, non-empty application database before WAL,
	// schema, permission, or index writes are allowed.
	if err := validateRequestLogMigrationTarget(db); err != nil {
		return err
	}

	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		return fmt.Errorf("检查迁移连接 busy_timeout 失败: %w", err)
	}
	if busyTimeout != sqliteBusyTimeoutMilliseconds {
		return fmt.Errorf("迁移连接 busy_timeout = %dms, want %dms", busyTimeout, sqliteBusyTimeoutMilliseconds)
	}

	if err := prepareRequestLogIndexes(db, databaseInitMigrateRequestLogIndexes); err != nil {
		return fmt.Errorf("迁移 request_log 索引失败: %w", err)
	}
	if err := ensureRequestLogRollupSchema(db); err != nil {
		return fmt.Errorf("创建 request_log 30 分钟汇总结构失败: %w", err)
	}
	if err := rebuildCurrentBeijingDayRequestLogRollup(db, time.Now(), output); err != nil {
		return fmt.Errorf("重建 request_log 30 分钟汇总失败: %w", err)
	}
	return nil
}

func validateRequestLogMigrationTarget(db *sql.DB) error {
	var objectType string
	err := db.QueryRow(`SELECT type FROM main.sqlite_schema WHERE name = 'request_log'`).Scan(&objectType)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("迁移目标缺少 request_log 表；请确认 HOME 和数据库路径")
	}
	if err != nil {
		return fmt.Errorf("检查迁移目标 request_log 表失败: %w", err)
	}
	if objectType != "table" {
		return fmt.Errorf("迁移目标 request_log 对象类型为 %q，不是 table", objectType)
	}

	var ready int
	err = db.QueryRow("SELECT 1 FROM main.request_log LIMIT 1").Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("迁移目标 request_log 表为空；拒绝迁移，请确认 HOME 和数据库路径")
	}
	if err != nil {
		return fmt.Errorf("读取迁移目标 request_log 失败: %w", err)
	}
	return nil
}

func sqliteDatabaseDSN(dbPath string) string {
	query := url.Values{}
	// Keep the existing non-file DSN behavior for cache/mode. Switching to a
	// file: URI would suddenly activate shared-cache semantics and is outside
	// this change. modernc still consumes _pragma for every physical connection.
	query.Set("cache", "shared")
	query.Set("mode", "rwc")
	query.Add("_pragma", fmt.Sprintf("busy_timeout=%d", sqliteBusyTimeoutMilliseconds))
	return dbPath + "?" + query.Encode()
}

func sqliteMigrationDatabaseDSN(dbPath string) string {
	uriPath := filepath.ToSlash(dbPath)
	if filepath.VolumeName(dbPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	databaseURL := url.URL{Scheme: "file", Path: uriPath}
	query := url.Values{}
	query.Set("mode", "rw")
	query.Add("_pragma", fmt.Sprintf("busy_timeout=%d", sqliteBusyTimeoutMilliseconds))
	databaseURL.RawQuery = query.Encode()
	return databaseURL.String()
}

func prepareRequestLogIndexes(db *sql.DB, mode databaseInitMode) error {
	missing, err := missingRequestLogIndexes(db)
	if err != nil {
		return fmt.Errorf("检查 request_log 索引失败: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}

	if mode == databaseInitMigrateRequestLogIndexes {
		fmt.Printf("⚠️  request_log 缺少 %d 个索引；每个索引都可能扫描完整历史表\n", len(missing))
		if err := createMissingRequestLogIndexes(
			db,
			func(name string) { fmt.Printf("⏳ 正在创建 request_log 索引 %s ...\n", name) },
			func(name string) { fmt.Printf("✅ request_log 索引 %s 创建完成\n", name) },
		); err != nil {
			return err
		}
		return nil
	}

	empty, err := requestLogTableIsEmpty(db)
	if err != nil {
		return fmt.Errorf("检查 request_log 是否为空失败: %w", err)
	}
	if empty {
		return ensureRequestLogIndexes(db)
	}

	return fmt.Errorf(
		"%w: missing %s; stop the service and run ./codeswitch-web %s as the service user",
		ErrRequestLogIndexMigrationRequired,
		strings.Join(missing, ", "),
		RequestLogIndexMigrationCommand,
	)
}

func requestLogTableIsEmpty(db *sql.DB) (bool, error) {
	var ready int
	err := db.QueryRow("SELECT 1 FROM request_log LIMIT 1").Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func ensureRequestLogRollupSchema(db *sql.DB) error {
	if db == nil {
		return errors.New("request_log rollup database is nil")
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS request_log_rollup_30m (
		bucket_start_utc TEXT NOT NULL,
		user_id TEXT NOT NULL,
		platform TEXT NOT NULL,
		total_requests INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		cache_create_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket_start_utc, user_id, platform)
	) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("创建 request_log_rollup_30m 表失败: %w", err)
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS request_log_rollup_30m_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		ready INTEGER NOT NULL DEFAULT 0 CHECK (ready IN (0, 1)),
		rebuilt_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return fmt.Errorf("创建 request_log_rollup_30m_state 表失败: %w", err)
	}

	if _, err := db.Exec(`CREATE TRIGGER IF NOT EXISTS request_log_rollup_30m_after_insert
	AFTER INSERT ON request_log
	WHEN DATETIME(NEW.created_at) IS NOT NULL
		OR DATETIME(SUBSTR(TRIM(COALESCE(NEW.created_at, '')), 1, 10)) IS NOT NULL
	BEGIN
		INSERT INTO request_log_rollup_30m (
			bucket_start_utc,
			user_id,
			platform,
			total_requests,
			input_tokens,
			output_tokens,
			reasoning_tokens,
			cache_create_tokens,
			cache_read_tokens
		) VALUES (
			CASE
				WHEN LENGTH(TRIM(COALESCE(NEW.created_at, ''))) = 10 OR DATETIME(NEW.created_at) IS NULL
					THEN STRFTIME('%Y-%m-%d %H:%M:%S', DATETIME(SUBSTR(TRIM(COALESCE(NEW.created_at, '')), 1, 10), '-8 hours'))
				ELSE STRFTIME('%Y-%m-%d %H:', DATETIME(NEW.created_at))
					|| CASE WHEN CAST(STRFTIME('%M', DATETIME(NEW.created_at)) AS INTEGER) < 30 THEN '00' ELSE '30' END
					|| ':00'
			END,
			COALESCE(NEW.user_id, ''),
			COALESCE(NEW.platform, ''),
			1,
			CASE WHEN COALESCE(NEW.exclude_from_total, 0) = 0 THEN COALESCE(NEW.input_tokens, 0) ELSE 0 END,
			CASE WHEN COALESCE(NEW.exclude_from_total, 0) = 0 THEN COALESCE(NEW.output_tokens, 0) ELSE 0 END,
			CASE WHEN COALESCE(NEW.exclude_from_total, 0) = 0 THEN COALESCE(NEW.reasoning_tokens, 0) ELSE 0 END,
			CASE WHEN COALESCE(NEW.exclude_from_total, 0) = 0 THEN COALESCE(NEW.cache_create_tokens, 0) ELSE 0 END,
			CASE WHEN COALESCE(NEW.exclude_from_total, 0) = 0 THEN COALESCE(NEW.cache_read_tokens, 0) ELSE 0 END
		)
		ON CONFLICT (bucket_start_utc, user_id, platform) DO UPDATE SET
			total_requests = total_requests + excluded.total_requests,
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
			cache_create_tokens = cache_create_tokens + excluded.cache_create_tokens,
			cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens;
	END`); err != nil {
		return fmt.Errorf("创建 request_log rollup trigger 失败: %w", err)
	}

	if _, err := db.Exec(`INSERT OR IGNORE INTO request_log_rollup_30m_state (id, ready, rebuilt_at) VALUES (?, 0, '')`, requestLogRollupStateID); err != nil {
		return fmt.Errorf("初始化 request_log rollup 状态失败: %w", err)
	}

	empty, err := requestLogTableIsEmpty(db)
	if err != nil {
		return fmt.Errorf("检查 request_log rollup 初始状态失败: %w", err)
	}
	if !empty {
		return nil
	}
	if _, err := db.Exec(`DELETE FROM request_log_rollup_30m`); err != nil {
		return fmt.Errorf("清理空库 request_log rollup 失败: %w", err)
	}
	if _, err := db.Exec(`UPDATE request_log_rollup_30m_state SET ready = 1, rebuilt_at = CURRENT_TIMESTAMP WHERE id = ?`, requestLogRollupStateID); err != nil {
		return fmt.Errorf("标记空库 request_log rollup ready 失败: %w", err)
	}
	return nil
}

func requireRequestLogRollupReady(db *sql.DB) error {
	if db == nil {
		return errors.New("request_log rollup database is nil")
	}
	var ready int
	err := db.QueryRow(`SELECT ready FROM request_log_rollup_30m_state WHERE id = ?`, requestLogRollupStateID).Scan(&ready)
	if err == nil && ready == 1 {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) && !strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return fmt.Errorf("读取 request_log rollup 状态失败: %w", err)
	}
	return fmt.Errorf(
		"%w: stop the service and run ./codeswitch-web %s as the service user",
		ErrRequestLogRollupMigrationRequired,
		RequestLogIndexMigrationCommand,
	)
}

func rebuildCurrentBeijingDayRequestLogRollup(db *sql.DB, now time.Time, output io.Writer) error {
	if db == nil {
		return errors.New("request_log rollup database is nil")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if output == nil {
		output = io.Discard
	}

	dayStart := startOfDay(now.In(beijingLocation))
	dayEnd := dayStart.Add(24 * time.Hour)
	queryStart := dayStart.UTC().Format(timeLayout)
	queryEnd := dayEnd.UTC().Format(timeLayout)

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开始 request_log rollup 重建事务失败: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM request_log_rollup_30m WHERE bucket_start_utc >= ? AND bucket_start_utc < ?`, queryStart, queryEnd); err != nil {
		return fmt.Errorf("清理北京时间当天 request_log rollup 失败: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO request_log_rollup_30m (
			bucket_start_utc,
			user_id,
			platform,
			total_requests,
			input_tokens,
			output_tokens,
			reasoning_tokens,
			cache_create_tokens,
			cache_read_tokens
		)
		SELECT
			CASE
				WHEN LENGTH(TRIM(COALESCE(created_at, ''))) = 10 OR DATETIME(created_at) IS NULL
					THEN STRFTIME('%Y-%m-%d %H:%M:%S', DATETIME(SUBSTR(TRIM(COALESCE(created_at, '')), 1, 10), '-8 hours'))
				ELSE STRFTIME('%Y-%m-%d %H:', DATETIME(created_at))
					|| CASE WHEN CAST(STRFTIME('%M', DATETIME(created_at)) AS INTEGER) < 30 THEN '00' ELSE '30' END
					|| ':00'
			END AS bucket_start_utc,
			COALESCE(user_id, '') AS user_id,
			COALESCE(platform, '') AS platform,
			COUNT(*) AS total_requests,
			COALESCE(SUM(CASE WHEN COALESCE(exclude_from_total, 0) = 0 THEN COALESCE(input_tokens, 0) ELSE 0 END), 0) AS input_tokens,
			COALESCE(SUM(CASE WHEN COALESCE(exclude_from_total, 0) = 0 THEN COALESCE(output_tokens, 0) ELSE 0 END), 0) AS output_tokens,
			COALESCE(SUM(CASE WHEN COALESCE(exclude_from_total, 0) = 0 THEN COALESCE(reasoning_tokens, 0) ELSE 0 END), 0) AS reasoning_tokens,
			COALESCE(SUM(CASE WHEN COALESCE(exclude_from_total, 0) = 0 THEN COALESCE(cache_create_tokens, 0) ELSE 0 END), 0) AS cache_create_tokens,
			COALESCE(SUM(CASE WHEN COALESCE(exclude_from_total, 0) = 0 THEN COALESCE(cache_read_tokens, 0) ELSE 0 END), 0) AS cache_read_tokens
		FROM request_log
		WHERE created_at >= ?
			AND created_at < ?
			AND (
				(DATETIME(created_at) IS NOT NULL AND DATETIME(created_at) >= ? AND DATETIME(created_at) < ?)
				OR (DATETIME(created_at) IS NULL AND SUBSTR(TRIM(COALESCE(created_at, '')), 1, 10) = ?)
			)
		GROUP BY bucket_start_utc, COALESCE(user_id, ''), COALESCE(platform, '')
	`, queryStart, queryEnd, queryStart, queryEnd, dayStart.Format("2006-01-02")); err != nil {
		return fmt.Errorf("聚合北京时间当天 request_log 失败: %w", err)
	}

	rebuiltAt := time.Now().UTC().Format(timeLayout)
	if _, err := tx.Exec(`
		INSERT INTO request_log_rollup_30m_state (id, ready, rebuilt_at)
		VALUES (?, 1, ?)
		ON CONFLICT (id) DO UPDATE SET ready = 1, rebuilt_at = excluded.rebuilt_at
	`, requestLogRollupStateID, rebuiltAt); err != nil {
		return fmt.Errorf("标记 request_log rollup ready 失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 request_log rollup 重建事务失败: %w", err)
	}
	if _, err := fmt.Fprintf(output, "request_log 30 分钟汇总已重建: beijing_day=%s buckets=48\n", dayStart.Format("2006-01-02")); err != nil {
		return fmt.Errorf("输出 request_log rollup 迁移结果失败: %w", err)
	}
	return nil
}

func hardenDatabaseFilePermissions(configDir string) {
	for _, name := range []string{"app.db", "app.db-wal", "app.db-shm"} {
		_ = os.Chmod(filepath.Join(configDir, name), 0o600)
	}
}

// ensureAppSettingsTable 确保通用应用设置表存在
func ensureAppSettingsTable() error {
	db, err := xdb.DB("default")
	if err != nil {
		return fmt.Errorf("获取数据库连接失败: %w", err)
	}

	const createAppSettingsSQL = `CREATE TABLE IF NOT EXISTS app_settings (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key TEXT UNIQUE NOT NULL,
		value TEXT
	)`
	if _, err := db.Exec(createAppSettingsSQL); err != nil {
		return fmt.Errorf("创建 app_settings 表失败: %w", err)
	}

	return nil
}
