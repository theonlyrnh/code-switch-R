package services

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daodao97/xgo/xdb"
)

const (
	TrafficCategoryRelayClient   = "relay_client"
	TrafficCategoryRelayUpstream = "relay_upstream"
	TrafficCategoryAdmin         = "admin"

	TrafficScopeLocal   = "local"
	TrafficScopePublic  = "public"
	TrafficScopeUnknown = "unknown"

	trafficDailyRetentionDays = 30
	trafficDayLayout          = "2006-01-02"
	legacyTrafficSeedKey      = "legacy_daily_seed_v1"
)

type TrafficEvent struct {
	TraceID       string    `json:"trace_id"`
	UserID        string    `json:"user_id"`
	Category      string    `json:"category"`
	Route         string    `json:"route"`
	Platform      string    `json:"platform"`
	Provider      string    `json:"provider"`
	AttemptIndex  int       `json:"attempt_index"`
	Retry         bool      `json:"retry"`
	NetworkScope  string    `json:"network_scope"`
	RequestBytes  int64     `json:"request_bytes"`
	ResponseBytes int64     `json:"response_bytes"`
	StatusCode    int       `json:"status_code"`
	CreatedAt     time.Time `json:"created_at"`
}

type TrafficBreakdown struct {
	IngressBytes int64 `json:"ingress_bytes"`
	EgressBytes  int64 `json:"egress_bytes"`
	Events       int64 `json:"events"`
}

type TrafficSummary struct {
	Since                 string           `json:"since"`
	GeneratedAt           string           `json:"generated_at"`
	RelayClient           TrafficBreakdown `json:"relay_client"`
	Upstream              TrafficBreakdown `json:"upstream"`
	Retry                 TrafficBreakdown `json:"retry"`
	Admin                 TrafficBreakdown `json:"admin"`
	AccountingDescription string           `json:"accounting_description"`
	DroppedEvents         int64            `json:"dropped_events"`
}

type TrafficService struct {
	db       *sql.DB
	events   chan TrafficEvent
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	stopping atomic.Bool
	dropped  atomic.Int64
}

func ensureTrafficSchema(db *sql.DB) error {
	if db == nil {
		return errors.New("traffic schema database is nil")
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS traffic_daily (
			day TEXT NOT NULL,
			user_id TEXT NOT NULL,
			category TEXT NOT NULL,
			network_scope TEXT NOT NULL,
			is_retry INTEGER NOT NULL DEFAULT 0,
			request_bytes INTEGER NOT NULL DEFAULT 0,
			response_bytes INTEGER NOT NULL DEFAULT 0,
			events INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, user_id, category, network_scope, is_retry)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS traffic_metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) WITHOUT ROWID`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return err
		}
	}
	return seedCurrentDayFromLegacyTraffic(db, time.Now())
}

func seedCurrentDayFromLegacyTraffic(db *sql.DB, now time.Time) error {
	day, start, end := trafficDayBounds(now)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var seededAt string
	if err := tx.QueryRow(`SELECT value FROM traffic_metadata WHERE key = ?`, legacyTrafficSeedKey).Scan(&seededAt); err == nil {
		return tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	hasTrafficEvents, err := sqliteTableExists(tx, "traffic_event")
	if err != nil {
		return err
	}
	if hasTrafficEvents {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO traffic_daily (
				day, user_id, category, network_scope, is_retry,
				request_bytes, response_bytes, events
			)
			SELECT ?, TRIM(user_id), category, 'public',
				CASE WHEN is_retry != 0 THEN 1 ELSE 0 END,
				COALESCE(SUM(request_bytes), 0),
				COALESCE(SUM(response_bytes), 0), COUNT(*)
			FROM traffic_event
			WHERE created_at >= ? AND created_at < ?
				AND LOWER(TRIM(network_scope)) = 'public'
				AND TRIM(COALESCE(user_id, '')) != ''
			GROUP BY TRIM(user_id), category,
				CASE WHEN is_retry != 0 THEN 1 ELSE 0 END`,
			day, start.UTC().Format(timeLayout), end.UTC().Format(timeLayout)); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`INSERT INTO traffic_metadata (key, value) VALUES (?, ?)`,
		legacyTrafficSeedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

func sqliteTableExists(tx *sql.Tx, table string) (bool, error) {
	var count int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func trafficDayBounds(value time.Time) (string, time.Time, time.Time) {
	local := value.In(beijingLocation)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, beijingLocation)
	return start.Format(trafficDayLayout), start, start.AddDate(0, 0, 1)
}

func NewTrafficService() (*TrafficService, error) {
	db, err := xdb.DB("default")
	if err != nil {
		return nil, err
	}
	if err := ensureTrafficSchema(db); err != nil {
		return nil, fmt.Errorf("initialize traffic schema: %w", err)
	}
	service := &TrafficService{
		db:     db,
		events: make(chan TrafficEvent, 8192),
		stop:   make(chan struct{}),
	}
	service.wg.Add(1)
	go service.eventWorker()
	return service, nil
}

func (s *TrafficService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.stopping.Store(true)
		close(s.stop)
		s.wg.Wait()
	})
}

func (s *TrafficService) Record(event TrafficEvent) {
	if s == nil || s.stopping.Load() {
		return
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	event.NetworkScope = normalizeTrafficScope(event.NetworkScope)
	event.UserID = strings.TrimSpace(event.UserID)
	if event.NetworkScope != TrafficScopePublic || event.UserID == "" {
		return
	}
	select {
	case s.events <- event:
	case <-s.stop:
	}
}

func (s *TrafficService) eventWorker() {
	defer s.wg.Done()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	cleanupTicker := time.NewTicker(24 * time.Hour)
	defer cleanupTicker.Stop()
	batch := make([]TrafficEvent, 0, 100)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insertTrafficEvents(batch); err != nil {
			fmt.Printf("更新 traffic_daily 失败: %v\n", err)
			s.dropped.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	for {
		select {
		case event := <-s.events:
			batch = append(batch, event)
			if len(batch) >= cap(batch) {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-cleanupTicker.C:
			s.cleanupOldTraffic()
		case <-s.stop:
			for {
				select {
				case event := <-s.events:
					batch = append(batch, event)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *TrafficService) insertTrafficEvents(events []TrafficEvent) error {
	type aggregateKey struct {
		Day      string
		UserID   string
		Category string
		Retry    int
	}
	type aggregateValue struct {
		RequestBytes  int64
		ResponseBytes int64
		Events        int64
	}
	aggregates := make(map[aggregateKey]aggregateValue)
	for _, event := range events {
		if normalizeTrafficScope(event.NetworkScope) != TrafficScopePublic {
			continue
		}
		userID := strings.TrimSpace(event.UserID)
		if userID == "" {
			continue
		}
		switch event.Category {
		case TrafficCategoryRelayClient, TrafficCategoryRelayUpstream, TrafficCategoryAdmin:
		default:
			continue
		}
		createdAt := event.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}
		day, _, _ := trafficDayBounds(createdAt)
		key := aggregateKey{
			Day:      day,
			UserID:   userID,
			Category: event.Category,
			Retry:    boolToInt(event.Retry),
		}
		value := aggregates[key]
		value.RequestBytes += maxInt64(event.RequestBytes, 0)
		value.ResponseBytes += maxInt64(event.ResponseBytes, 0)
		value.Events++
		aggregates[key] = value
	}
	if len(aggregates) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.Prepare(`
		INSERT INTO traffic_daily (
			day, user_id, category, network_scope, is_retry,
			request_bytes, response_bytes, events
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(day, user_id, category, network_scope, is_retry) DO UPDATE SET
			request_bytes = request_bytes + excluded.request_bytes,
			response_bytes = response_bytes + excluded.response_bytes,
			events = events + excluded.events`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for key, value := range aggregates {
		if _, err := statement.Exec(
			key.Day, key.UserID, key.Category, TrafficScopePublic, key.Retry,
			value.RequestBytes, value.ResponseBytes, value.Events,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *TrafficService) cleanupOldTraffic() {
	cutoff, _, _ := trafficDayBounds(time.Now().AddDate(0, 0, -trafficDailyRetentionDays))
	_, _ = s.db.Exec(`DELETE FROM traffic_daily WHERE day < ?`, cutoff)
}

func (s *TrafficService) SummaryTodayForUser(userID string) (TrafficSummary, error) {
	if s == nil || s.db == nil {
		return TrafficSummary{}, errors.New("traffic service is unavailable")
	}
	now := time.Now()
	day, since, _ := trafficDayBounds(now)
	summary := TrafficSummary{
		Since:                 since.Format(time.RFC3339),
		GeneratedAt:           now.Format(time.RFC3339),
		AccountingDescription: "Only explicitly public Code Switch application-body traffic is counted. Values exclude local or unknown routes and exclude HTTP/TLS overhead.",
		DroppedEvents:         s.dropped.Load(),
	}

	rows, err := s.db.Query(`
		SELECT category, is_retry, request_bytes, response_bytes, events
		FROM traffic_daily
		WHERE day = ? AND user_id = ? AND network_scope = ?`,
		day, strings.TrimSpace(userID), TrafficScopePublic)
	if err != nil {
		return TrafficSummary{}, err
	}
	for rows.Next() {
		var category string
		var retry int
		var requestBytes, responseBytes, events int64
		if err := rows.Scan(&category, &retry, &requestBytes, &responseBytes, &events); err != nil {
			_ = rows.Close()
			return TrafficSummary{}, err
		}
		assignPublicTrafficBreakdown(&summary, category, retry != 0, requestBytes, responseBytes, events)
	}
	if err := rows.Close(); err != nil {
		return TrafficSummary{}, err
	}
	return summary, nil
}

func assignPublicTrafficBreakdown(
	summary *TrafficSummary,
	category string,
	retry bool,
	requestBytes, responseBytes, events int64,
) {
	if summary == nil {
		return
	}
	switch category {
	case TrafficCategoryRelayClient:
		summary.RelayClient.IngressBytes += requestBytes
		summary.RelayClient.EgressBytes += responseBytes
		summary.RelayClient.Events += events
	case TrafficCategoryRelayUpstream:
		summary.Upstream.IngressBytes += responseBytes
		summary.Upstream.EgressBytes += requestBytes
		summary.Upstream.Events += events
		if retry {
			summary.Retry.IngressBytes += responseBytes
			summary.Retry.EgressBytes += requestBytes
			summary.Retry.Events += events
		}
	case TrafficCategoryAdmin:
		summary.Admin.IngressBytes += requestBytes
		summary.Admin.EgressBytes += responseBytes
		summary.Admin.Events += events
	}
}

func NewTrafficTraceID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func normalizeTrafficScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case TrafficScopeLocal:
		return TrafficScopeLocal
	case TrafficScopePublic:
		return TrafficScopePublic
	default:
		return TrafficScopeUnknown
	}
}

var localTrafficAddresses struct {
	sync.Once
	values map[netip.Addr]struct{}
}

type trafficHostCacheEntry struct {
	scope     string
	expiresAt time.Time
}

var trafficHostCache = struct {
	sync.Mutex
	values map[string]trafficHostCacheEntry
}{values: make(map[string]trafficHostCacheEntry)}

func ClassifyNetworkScope(raw string) string {
	host := trafficHost(raw)
	if host == "" {
		return TrafficScopeUnknown
	}
	if strings.EqualFold(host, "localhost") {
		return TrafficScopeLocal
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return classifyTrafficIP(ip)
	}

	now := time.Now()
	trafficHostCache.Lock()
	if cached, ok := trafficHostCache.values[host]; ok && now.Before(cached.expiresAt) {
		trafficHostCache.Unlock()
		return cached.scope
	}
	trafficHostCache.Unlock()

	scope := TrafficScopePublic
	addresses, err := net.LookupIP(host)
	if err != nil || len(addresses) == 0 {
		scope = TrafficScopeUnknown
	} else {
		for _, address := range addresses {
			if ip, ok := netip.AddrFromSlice(address); ok && classifyTrafficIP(ip.Unmap()) == TrafficScopeLocal {
				scope = TrafficScopeLocal
				break
			}
		}
	}
	trafficHostCache.Lock()
	trafficHostCache.values[host] = trafficHostCacheEntry{scope: scope, expiresAt: now.Add(5 * time.Minute)}
	trafficHostCache.Unlock()
	return scope
}

func trafficHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
		return strings.ToLower(parsed.Hostname())
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return strings.ToLower(strings.Trim(host, "[]"))
	}
	return strings.ToLower(strings.Trim(raw, "[]"))
}

func classifyTrafficIP(ip netip.Addr) string {
	if !ip.IsValid() {
		return TrafficScopeUnknown
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return TrafficScopeLocal
	}
	localTrafficAddresses.Do(func() {
		localTrafficAddresses.values = make(map[netip.Addr]struct{})
		interfaces, _ := net.Interfaces()
		for _, item := range interfaces {
			addresses, _ := item.Addrs()
			for _, address := range addresses {
				prefix, err := netip.ParsePrefix(address.String())
				if err == nil {
					localTrafficAddresses.values[prefix.Addr().Unmap()] = struct{}{}
				}
			}
		}
	})
	if _, ok := localTrafficAddresses.values[ip]; ok {
		return TrafficScopeLocal
	}
	return TrafficScopePublic
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}
