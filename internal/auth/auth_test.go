package auth

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"scruples/internal/db"
)

func TestSeedAdmin(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	var buf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(origOut) })

	// Empty password on fresh DB generates a random 16-byte hex password and logs it once.
	if err := SeedAdmin(database, "", false); err != nil {
		t.Fatalf("SeedAdmin initial: %v", err)
	}

	var hash1 string
	var isAdmin int
	if err := database.QueryRow(`SELECT password_hash, is_admin FROM users WHERE username = 'nick'`).Scan(&hash1, &isAdmin); err != nil {
		t.Fatalf("query admin: %v", err)
	}
	if isAdmin != 1 {
		t.Fatalf("is_admin = %d, want 1", isAdmin)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash1), []byte("changeme-admin")) == nil {
		t.Fatal("admin password must not use hardcoded fallback")
	}
	logged := buf.String()
	const marker = "generated password: "
	idx := strings.Index(logged, marker)
	if idx < 0 {
		t.Fatalf("expected generated password in log, got %q", logged)
	}
	genPass := strings.TrimSpace(logged[idx+len(marker):])
	if len(genPass) != 32 {
		t.Fatalf("generated password len = %d, want 32 hex chars (%q)", len(genPass), genPass)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash1), []byte(genPass)); err != nil {
		t.Fatalf("logged password does not match stored hash: %v", err)
	}

	// Second call with empty password leaves password alone and logs nothing.
	buf.Reset()
	if err := SeedAdmin(database, "", false); err != nil {
		t.Fatalf("SeedAdmin second: %v", err)
	}
	var hash2 string
	if err := database.QueryRow(`SELECT password_hash FROM users WHERE username = 'nick'`).Scan(&hash2); err != nil {
		t.Fatalf("query admin second: %v", err)
	}
	if hash2 != hash1 {
		t.Fatal("SeedAdmin overwrote existing admin password when reset=false")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no log on subsequent SeedAdmin, got %q", buf.String())
	}

	// Reset with explicit password updates the stored hash.
	if err := SeedAdmin(database, "recovery-pass", true); err != nil {
		t.Fatalf("SeedAdmin reset: %v", err)
	}
	var hash3 string
	if err := database.QueryRow(`SELECT password_hash FROM users WHERE username = 'nick'`).Scan(&hash3); err != nil {
		t.Fatalf("query admin after reset: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash3), []byte("recovery-pass")); err != nil {
		t.Fatalf("reset password did not match: %v", err)
	}
}
