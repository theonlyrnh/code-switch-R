package services

import (
	"database/sql"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClassifyNetworkScopeSeparatesLoopbackAndPublic(t *testing.T) {
	if got := ClassifyNetworkScope("http://localhost:18100/responses"); got != TrafficScopeLocal {
		t.Fatalf("localhost scope = %q, want local", got)
	}
	if got := ClassifyNetworkScope("127.0.0.1:8080"); got != TrafficScopeLocal {
		t.Fatalf("loopback scope = %q, want local", got)
	}
	if got := ClassifyNetworkScope("203.0.113.10"); got != TrafficScopePublic {
		t.Fatalf("external scope = %q, want public", got)
	}
}

func TestUpstreamTrafficAttemptCountsRetriesAndDirections(t *testing.T) {
	requestLog := &ReqeustLog{
		TrafficTraceID: "trace-a",
		UserID:         "user-a",
		Platform:       "openai-responses",
		traffic:        &requestTrafficState{},
	}
	metadata := &upstreamTrafficMetadata{
		requestLog: requestLog,
		provider:   "provider-a",
		targetURL:  "https://203.0.113.10/responses",
	}

	for index := 0; index < 2; index++ {
		attempt := metadata.begin()
		request, err := http.NewRequest(http.MethodPost, metadata.targetURL, io.NopCloser(strings.NewReader("request")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		attempt.wrapRequest(request)
		if _, err := io.ReadAll(request.Body); err != nil {
			t.Fatalf("read request body: %v", err)
		}
		response := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("response"))}
		attempt.wrapResponse(response)
		if _, err := io.ReadAll(response.Body); err != nil {
			t.Fatalf("read response body: %v", err)
		}
		_ = response.Body.Close()
	}

	if requestLog.UpstreamAttempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", requestLog.UpstreamAttempts)
	}
	if requestLog.UpstreamRequestBytes != 14 || requestLog.UpstreamResponseBytes != 16 {
		t.Fatalf("upstream bytes = (%d,%d), want (14,16)", requestLog.UpstreamRequestBytes, requestLog.UpstreamResponseBytes)
	}
	if requestLog.RetryRequestBytes != 7 || requestLog.RetryResponseBytes != 8 {
		t.Fatalf("retry bytes = (%d,%d), want (7,8)", requestLog.RetryRequestBytes, requestLog.RetryResponseBytes)
	}
	if requestLog.PublicEgressBytes != 14 || requestLog.PublicIngressBytes != 16 {
		t.Fatalf("public bytes = ingress %d egress %d, want 16/14", requestLog.PublicIngressBytes, requestLog.PublicEgressBytes)
	}
}

func TestTrafficSummaryCountsOnlyPublicTrafficForCurrentUser(t *testing.T) {
	db := openTrafficTestDB(t)
	service := &TrafficService{db: db}
	now := time.Now()
	events := []TrafficEvent{
		{UserID: "user-a", Category: TrafficCategoryRelayClient, NetworkScope: TrafficScopeLocal, RequestBytes: 100, ResponseBytes: 20, CreatedAt: now},
		{UserID: "user-a", Category: TrafficCategoryRelayClient, NetworkScope: TrafficScopePublic, RequestBytes: 120, ResponseBytes: 40, CreatedAt: now},
		{UserID: "user-a", Category: TrafficCategoryRelayUpstream, Provider: "provider-a", NetworkScope: TrafficScopePublic, RequestBytes: 110, ResponseBytes: 30, CreatedAt: now},
		{UserID: "user-a", Category: TrafficCategoryRelayUpstream, Provider: "provider-b", NetworkScope: TrafficScopePublic, Retry: true, RequestBytes: 90, ResponseBytes: 10, CreatedAt: now},
		{UserID: "user-a", Category: TrafficCategoryAdmin, Route: "ConsoleService.GetLogUpdates", NetworkScope: TrafficScopePublic, RequestBytes: 5, ResponseBytes: 50, CreatedAt: now},
		{UserID: "user-a", Category: TrafficCategoryAdmin, NetworkScope: TrafficScopeUnknown, RequestBytes: 500, ResponseBytes: 500, CreatedAt: now},
		{UserID: "other-user", Category: TrafficCategoryAdmin, NetworkScope: TrafficScopePublic, RequestBytes: 999, ResponseBytes: 999, CreatedAt: now},
		{UserID: "", Category: TrafficCategoryAdmin, NetworkScope: TrafficScopePublic, RequestBytes: 888, ResponseBytes: 888, CreatedAt: now},
		{UserID: "user-a", Category: TrafficCategoryAdmin, NetworkScope: TrafficScopePublic, RequestBytes: 777, ResponseBytes: 777, CreatedAt: now.AddDate(0, 0, -1)},
	}
	if err := service.insertTrafficEvents(events); err != nil {
		t.Fatalf("insert traffic events: %v", err)
	}

	summary, err := service.SummaryTodayForUser("user-a")
	if err != nil {
		t.Fatalf("SummaryTodayForUser: %v", err)
	}
	if summary.RelayClient.IngressBytes != 120 || summary.RelayClient.EgressBytes != 40 || summary.RelayClient.Events != 1 {
		t.Fatalf("relay client = %+v", summary.RelayClient)
	}
	if summary.Upstream.IngressBytes != 40 || summary.Upstream.EgressBytes != 200 || summary.Upstream.Events != 2 {
		t.Fatalf("upstream = %+v", summary.Upstream)
	}
	if summary.Retry.IngressBytes != 10 || summary.Retry.EgressBytes != 90 || summary.Retry.Events != 1 {
		t.Fatalf("retry = %+v", summary.Retry)
	}
	if summary.Admin.IngressBytes != 5 || summary.Admin.EgressBytes != 50 || summary.Admin.Events != 1 {
		t.Fatalf("admin = %+v", summary.Admin)
	}

	otherSummary, err := service.SummaryTodayForUser("other-user")
	if err != nil {
		t.Fatalf("SummaryTodayForUser other user: %v", err)
	}
	if otherSummary.Admin.IngressBytes != 999 || otherSummary.Admin.EgressBytes != 999 || otherSummary.Admin.Events != 1 {
		t.Fatalf("other user traffic = %+v", otherSummary.Admin)
	}

	var nonPublicRows, emptyUserRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM traffic_daily WHERE network_scope != ?`, TrafficScopePublic).Scan(&nonPublicRows); err != nil {
		t.Fatalf("count non-public daily rows: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM traffic_daily WHERE user_id = ''`).Scan(&emptyUserRows); err != nil {
		t.Fatalf("count unattributed daily rows: %v", err)
	}
	if nonPublicRows != 0 || emptyUserRows != 0 {
		t.Fatalf("non-public or unattributed rows were stored: non_public=%d empty_user=%d", nonPublicRows, emptyUserRows)
	}
}

func TestTrafficSummaryUsesDailyPrimaryKeyLookups(t *testing.T) {
	db := openTrafficTestDB(t)
	assertTrafficQueryUsesPrimaryKey(t, db,
		`SELECT category, is_retry, request_bytes, response_bytes, events
		 FROM traffic_daily WHERE day = ? AND user_id = ? AND network_scope = ?`,
		"2026-07-28", "user-a", TrafficScopePublic)
}

func assertTrafficQueryUsesPrimaryKey(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain traffic query: %v", err)
	}
	defer rows.Close()
	var details strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan traffic query plan: %v", err)
		}
		details.WriteString(detail)
		details.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read traffic query plan: %v", err)
	}
	if !strings.Contains(details.String(), "USING PRIMARY KEY") {
		t.Fatalf("traffic query does not use primary key:\n%s", details.String())
	}
}

func TestTrafficSchemaDoesNotCreateLegacyDetailTables(t *testing.T) {
	db := openTrafficTestDB(t)
	for _, table := range []string{
		"traffic_event", "network_interface_sample",
		"network_interface_daily", "network_interface_counter",
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("check legacy table %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("legacy detail table %s was created", table)
		}
	}
}

func TestTrafficSchemaSeedsCurrentDayLegacyDetailsOnce(t *testing.T) {
	db := openRequestLogSchemaTestDB(t)
	if _, err := db.Exec(`
		CREATE TABLE traffic_event (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT,
			category TEXT NOT NULL,
			is_retry INTEGER DEFAULT 0,
			network_scope TEXT NOT NULL,
			request_bytes INTEGER DEFAULT 0,
			response_bytes INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		t.Fatalf("create legacy traffic tables: %v", err)
	}
	now := time.Now()
	createdAt := now.UTC().Format(timeLayout)
	if _, err := db.Exec(`
		INSERT INTO traffic_event (
			user_id, category, is_retry, network_scope,
			request_bytes, response_bytes, created_at
		) VALUES
			('user-a', 'relay_client', 0, 'public', 12, 34, ?),
			('user-a', 'relay_client', 0, 'local', 100, 200, ?)`, createdAt, createdAt); err != nil {
		t.Fatalf("insert legacy traffic: %v", err)
	}
	if err := ensureTrafficSchema(db); err != nil {
		t.Fatalf("ensure traffic schema first run: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO traffic_event (
			user_id, category, is_retry, network_scope,
			request_bytes, response_bytes, created_at
		) VALUES ('user-a', 'relay_client', 0, 'public', 1000, 2000, ?)`, createdAt); err != nil {
		t.Fatalf("insert legacy traffic after migration marker: %v", err)
	}
	if err := ensureTrafficSchema(db); err != nil {
		t.Fatalf("ensure traffic schema second run: %v", err)
	}
	service := &TrafficService{db: db}
	summary, err := service.SummaryTodayForUser("user-a")
	if err != nil {
		t.Fatalf("read migrated daily summary: %v", err)
	}
	if summary.RelayClient.IngressBytes != 12 || summary.RelayClient.EgressBytes != 34 || summary.RelayClient.Events != 1 {
		t.Fatalf("legacy public request traffic was not seeded exactly once: %+v", summary.RelayClient)
	}
	var marker string
	if err := db.QueryRow(`SELECT value FROM traffic_metadata WHERE key = ?`, legacyTrafficSeedKey).Scan(&marker); err != nil {
		t.Fatalf("read legacy migration marker: %v", err)
	}
	if marker == "" {
		t.Fatal("legacy migration marker is empty")
	}
}

func openTrafficTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openRequestLogSchemaTestDB(t)
	if err := ensureTrafficSchema(db); err != nil {
		t.Fatalf("ensureTrafficSchema: %v", err)
	}
	return db
}
