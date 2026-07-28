package main

import (
	"codeswitch/services"
	"context"
	"testing"
	"time"
)

func TestUserScopedConsoleServiceReturnsOnlyPoolAttemptUpdates(t *testing.T) {
	attempts := services.NewPoolAttemptLogService()
	stamp := time.Now()
	attempts.Add("user-a", services.ConsoleLog{Timestamp: stamp, Level: "ERROR", Message: "first"})
	scoped := &userScopedConsoleService{poolAttemptLogs: attempts}
	ctx := contextWithAuthenticatedUser(context.Background(), &services.AuthenticatedUser{ID: "user-a", Username: "alice"})

	initial, err := scoped.GetLogUpdates(ctx, services.ConsoleLogCursor{}, 200)
	if err != nil {
		t.Fatalf("initial console update: %v", err)
	}
	if len(initial.Logs) != 1 || initial.Logs[0].Message != "first" {
		t.Fatalf("initial batch = %#v", initial)
	}

	attempts.Add("user-a", services.ConsoleLog{Timestamp: stamp, Level: "ERROR", Message: "second"})
	updates, err := scoped.GetLogUpdates(ctx, initial.Cursor, 200)
	if err != nil {
		t.Fatalf("incremental console update: %v", err)
	}
	if len(updates.Logs) != 1 || updates.Logs[0].Message != "second" {
		t.Fatalf("incremental batch = %#v, want only second", updates)
	}
}
