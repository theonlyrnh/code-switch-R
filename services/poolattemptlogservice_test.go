package services

import (
	"testing"
	"time"
)

func TestPoolAttemptLogServiceListAfterUsesSequenceCursor(t *testing.T) {
	service := NewPoolAttemptLogService()
	stamp := time.Now()
	service.Add("user-a", ConsoleLog{Timestamp: stamp, Message: "first"})
	service.Add("user-a", ConsoleLog{Timestamp: stamp, Message: "second"})

	initial, cursor := service.ListAfter("user-a", 0, 200, time.Time{})
	if len(initial) != 2 || initial[0].Message != "first" || initial[1].Message != "second" {
		t.Fatalf("initial logs = %#v", initial)
	}

	service.Add("user-a", ConsoleLog{Timestamp: stamp, Message: "third"})
	updates, next := service.ListAfter("user-a", cursor, 200, time.Time{})
	if len(updates) != 1 || updates[0].Message != "third" {
		t.Fatalf("updates = %#v, want only third", updates)
	}
	if next <= cursor {
		t.Fatalf("next cursor = %d, want greater than %d", next, cursor)
	}
}

func TestConsoleServiceGetLogUpdatesIsIncremental(t *testing.T) {
	service := &ConsoleService{logs: make([]ConsoleLog, 0, 1000), maxLogs: 1000}
	service.addLog("INFO", "first")
	initial := service.GetLogUpdates(ConsoleLogCursor{}, 200)
	if len(initial.Logs) != 1 || initial.Logs[0].Message != "first" {
		t.Fatalf("initial batch = %#v", initial)
	}

	service.addLog("INFO", "second")
	updates := service.GetLogUpdates(initial.Cursor, 200)
	if len(updates.Logs) != 1 || updates.Logs[0].Message != "second" {
		t.Fatalf("update batch = %#v", updates)
	}
}
