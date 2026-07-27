package filebadara

import (
	"fmt"
	"os"
	"strings"
)

// ReadPasswordFile reads a password from a regular owner-only file.
// A trailing CR/LF sequence is ignored.
func ReadPasswordFile(filename string) (string, error) {
	if filename == "" {
		return "", nil
	}

	info, err := os.Stat(filename)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("password file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("password file must not be accessible by group or others; run chmod 600 on it")
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	password := strings.TrimRight(string(data), "\r\n")
	if password == "" {
		return "", fmt.Errorf("password file is empty")
	}
	return password, nil
}
