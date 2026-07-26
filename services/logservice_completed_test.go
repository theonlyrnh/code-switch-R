package services

import (
	"database/sql"
	"testing"
	"time"

	"github.com/daodao97/xgo/xdb"
)

func completedLogTestDB(t *testing.T) *sql.DB {
	t.Helper()
	setupCostServiceTestDB(t)
	db, err := xdb.DB("default")
	if err != nil {
		t.Fatalf("xdb.DB failed: %v", err)
	}
	return db
}

func insertCompletedLogForTest(t *testing.T, db *sql.DB, userID string, provider string) int64 {
	t.Helper()
	result, err := db.Exec(`
		INSERT INTO request_log (
			user_id, platform, model, provider, http_code, created_at
		) VALUES (?, ?, ?, ?, ?, ?)
	`, userID, "claude", "claude-sonnet", provider, 200, time.Now().UTC().Format(timeLayout))
	if err != nil {
		t.Fatalf("insert completed request log: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("completed request log id: %v", err)
	}
	return id
}

func TestListCompletedRequestLogsInitialCapsAt105AndOrdersDescending(t *testing.T) {
	db := completedLogTestDB(t)
	userAIDs := make([]int64, 0, 110)
	for i := 0; i < 110; i++ {
		userAIDs = append(userAIDs, insertCompletedLogForTest(t, db, "user-a", "provider-a"))
	}
	insertCompletedLogForTest(t, db, "user-b", "provider-b")

	logs, err := NewLogService().ListCompletedRequestLogsForUser("user-a", -1, 1000)
	if err != nil {
		t.Fatalf("ListCompletedRequestLogsForUser: %v", err)
	}
	if len(logs) != maxCompletedRequestLogs {
		t.Fatalf("completed logs count = %d, want %d", len(logs), maxCompletedRequestLogs)
	}
	if logs[0].ID != userAIDs[len(userAIDs)-1] {
		t.Fatalf("first completed id = %d, want %d", logs[0].ID, userAIDs[len(userAIDs)-1])
	}
	if logs[len(logs)-1].ID != userAIDs[len(userAIDs)-maxCompletedRequestLogs] {
		t.Fatalf("last completed id = %d, want %d", logs[len(logs)-1].ID, userAIDs[len(userAIDs)-maxCompletedRequestLogs])
	}
	for index, logEntry := range logs {
		if logEntry.UserID != "user-a" {
			t.Fatalf("completed log %d belongs to %q, want user-a", index, logEntry.UserID)
		}
		if logEntry.Status != requestLogStatusCompleted {
			t.Fatalf("completed log %d status = %q, want completed", index, logEntry.Status)
		}
		if index > 0 && logs[index-1].ID <= logEntry.ID {
			t.Fatalf("completed ids are not descending at %d: %d <= %d", index, logs[index-1].ID, logEntry.ID)
		}
	}
}

func TestListCompletedRequestLogsAfterIDReturnsOnlyNewerRows(t *testing.T) {
	db := completedLogTestDB(t)
	firstID := insertCompletedLogForTest(t, db, "user-a", "provider-a-1")
	insertCompletedLogForTest(t, db, "user-b", "provider-b")
	secondID := insertCompletedLogForTest(t, db, "user-a", "provider-a-2")
	thirdID := insertCompletedLogForTest(t, db, "user-a", "provider-a-3")

	logs, err := NewLogService().ListCompletedRequestLogsForUser("user-a", firstID, maxCompletedRequestLogs)
	if err != nil {
		t.Fatalf("ListCompletedRequestLogsForUser after id: %v", err)
	}
	if len(logs) != 2 || logs[0].ID != thirdID || logs[1].ID != secondID {
		t.Fatalf("incremental completed logs = %#v, want ids [%d %d]", logs, thirdID, secondID)
	}
	for _, logEntry := range logs {
		if logEntry.ID <= firstID || logEntry.UserID != "user-a" {
			t.Fatalf("incremental completed log = %#v, want newer user-a row", logEntry)
		}
	}

	empty, err := NewLogService().ListCompletedRequestLogsForUser("user-a", thirdID, maxCompletedRequestLogs)
	if err != nil {
		t.Fatalf("ListCompletedRequestLogsForUser empty increment: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("completed logs after newest id = %#v, want empty", empty)
	}
}

func TestActiveAndCompletedRequestLogsKeepSeparateQuotaAndUserScope(t *testing.T) {
	db := completedLogTestDB(t)
	for i := 0; i < maxCompletedRequestLogs; i++ {
		insertCompletedLogForTest(t, db, "user-a", "completed-provider-a")
	}
	insertCompletedLogForTest(t, db, "user-b", "completed-provider-b")

	previousTracker := defaultActiveRequestTracker
	defaultActiveRequestTracker = newActiveRequestTracker()
	t.Cleanup(func() {
		defaultActiveRequestTracker = previousTracker
	})
	for i := 0; i < 3; i++ {
		defaultActiveRequestTracker.Start(&ReqeustLog{
			UserID:   "user-a",
			Platform: "claude",
			Provider: "active-provider-a",
		}, time.Now().Add(time.Duration(i)*time.Millisecond))
	}
	defaultActiveRequestTracker.Start(&ReqeustLog{
		UserID:   "user-b",
		Platform: "claude",
		Provider: "active-provider-b",
	}, time.Now())

	service := NewLogService()
	activeA, err := service.ListActiveRequestLogsForUser("user-a")
	if err != nil {
		t.Fatalf("ListActiveRequestLogsForUser user-a: %v", err)
	}
	completedA, err := service.ListCompletedRequestLogsForUser("user-a", -1, maxCompletedRequestLogs)
	if err != nil {
		t.Fatalf("ListCompletedRequestLogsForUser user-a: %v", err)
	}
	if len(activeA) != 3 || len(completedA) != maxCompletedRequestLogs {
		t.Fatalf("user-a active/completed counts = %d/%d, want 3/%d", len(activeA), len(completedA), maxCompletedRequestLogs)
	}
	for _, logEntry := range activeA {
		if logEntry.UserID != "user-a" || logEntry.ID >= 0 {
			t.Fatalf("user-a active log = %#v, want negative-id user-a row", logEntry)
		}
	}
	for _, logEntry := range completedA {
		if logEntry.UserID != "user-a" || logEntry.ID <= 0 {
			t.Fatalf("user-a completed log = %#v, want positive-id user-a row", logEntry)
		}
	}

	activeB, err := service.ListActiveRequestLogsForUser("user-b")
	if err != nil {
		t.Fatalf("ListActiveRequestLogsForUser user-b: %v", err)
	}
	completedB, err := service.ListCompletedRequestLogsForUser("user-b", -1, maxCompletedRequestLogs)
	if err != nil {
		t.Fatalf("ListCompletedRequestLogsForUser user-b: %v", err)
	}
	if len(activeB) != 1 || len(completedB) != 1 {
		t.Fatalf("user-b active/completed counts = %d/%d, want 1/1", len(activeB), len(completedB))
	}
}

func TestLogServiceListMethodsRejectEmptyUserID(t *testing.T) {
	service := NewLogService()
	if _, err := service.ListActiveRequestLogsForUser(" "); err == nil {
		t.Fatal("ListActiveRequestLogsForUser accepted an empty user id")
	}
	if _, err := service.ListCompletedRequestLogsForUser(" ", -1, maxCompletedRequestLogs); err == nil {
		t.Fatal("ListCompletedRequestLogsForUser accepted an empty user id")
	}
}
