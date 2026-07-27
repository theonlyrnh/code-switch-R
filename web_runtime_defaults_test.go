package main

import "testing"

func TestDefaultListenersAreLocalhostOnly(t *testing.T) {
	if defaultAdminAddr != "127.0.0.1:8080" {
		t.Fatalf("default admin address = %q, want localhost only", defaultAdminAddr)
	}
	if defaultRelayAddr != "127.0.0.1:18100" {
		t.Fatalf("default relay address = %q, want localhost only", defaultRelayAddr)
	}
}
