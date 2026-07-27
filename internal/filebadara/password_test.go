package filebadara

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadPasswordFile checks that a file written by a normal editor or shell
// redirect works, since those always leave a trailing newline behind.
//
// Scenario: a 0600 file contains "secret\n".
// Expect: the password reads back as "secret", without the newline.
func TestReadPasswordFile(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(filename, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	password, err := ReadPasswordFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if password != "secret" {
		t.Fatalf("got %q, want secret", password)
	}
}

// TestReadPasswordFileRejectsOpenPermissions checks that a leakable password
// file stops the server instead of being used, since every local account could
// otherwise read it.
//
// Scenario: the password file is mode 0644.
// Expect: an error rather than a password.
func TestReadPasswordFileRejectsOpenPermissions(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(filename, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadPasswordFile(filename); err == nil {
		t.Fatal("expected insecure permissions to be rejected")
	}
}
