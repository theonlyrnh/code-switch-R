package main

import (
	"errors"
	"testing"

	"codeswitch/services"
	_ "modernc.org/sqlite"
)

func TestRunMaintenanceCommand(t *testing.T) {
	t.Run("no command starts service", func(t *testing.T) {
		called := false
		handled, err := runMaintenanceCommand(nil, func() error {
			called = true
			return nil
		})
		if err != nil || handled || called {
			t.Fatalf("handled=%t called=%t err=%v, want false false nil", handled, called, err)
		}
	})

	t.Run("migration command runs once", func(t *testing.T) {
		calls := 0
		handled, err := runMaintenanceCommand([]string{services.RequestLogIndexMigrationCommand}, func() error {
			calls++
			return nil
		})
		if err != nil || !handled || calls != 1 {
			t.Fatalf("handled=%t calls=%d err=%v, want true 1 nil", handled, calls, err)
		}
	})

	t.Run("migration error is propagated", func(t *testing.T) {
		wantErr := errors.New("migration failed")
		handled, err := runMaintenanceCommand([]string{services.RequestLogIndexMigrationCommand}, func() error {
			return wantErr
		})
		if !handled || !errors.Is(err, wantErr) {
			t.Fatalf("handled=%t err=%v, want handled migration error", handled, err)
		}
	})

	for _, args := range [][]string{{"unknown"}, {services.RequestLogIndexMigrationCommand, "extra"}} {
		t.Run("rejects invalid arguments", func(t *testing.T) {
			called := false
			handled, err := runMaintenanceCommand(args, func() error {
				called = true
				return nil
			})
			if !handled || err == nil || called {
				t.Fatalf("args=%v handled=%t called=%t err=%v, want rejected without migration", args, handled, called, err)
			}
		})
	}
}
