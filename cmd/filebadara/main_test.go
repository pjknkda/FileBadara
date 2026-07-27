package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjknkda/filebadara/internal/filebadara"
)

// TestParseConfigDefaults checks the configuration a bare `filebadara` runs
// with, since those defaults are what most deployments inherit.
//
// Scenario: no arguments at all.
// Expect: plain HTTP on :80, no password, and the standard sharing lifetime.
func TestParseConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("got no config")
	}
	if cfg.addr != ":80" {
		t.Errorf("addr is %q, want :80", cfg.addr)
	}
	if cfg.domain != "" {
		t.Errorf("domain is %q, want empty", cfg.domain)
	}
	if cfg.password != "" {
		t.Errorf("password is %q, want empty", cfg.password)
	}
	if cfg.certCache != ".filebadara-certs" {
		t.Errorf("certCache is %q, want .filebadara-certs", cfg.certCache)
	}
	if cfg.ttl != filebadara.DefaultTransferTTL {
		t.Errorf("ttl is %s, want %s", cfg.ttl, filebadara.DefaultTransferTTL)
	}
}

// TestParseConfigAcceptsFlags checks that each flag lands in the field the
// server reads.
//
// Scenario: every flag is set at once, with padding around the domain.
// Expect: the values arrive intact and the domain is trimmed.
func TestParseConfigAcceptsFlags(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-addr", "127.0.0.1:8080",
		"-domain", "  badara.example.com  ",
		"-cert-cache", "/var/lib/filebadara/certs",
		"-ttl", "90s",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "127.0.0.1:8080" {
		t.Errorf("addr is %q", cfg.addr)
	}
	if cfg.domain != "badara.example.com" {
		t.Errorf("domain is %q, want it trimmed", cfg.domain)
	}
	if cfg.certCache != "/var/lib/filebadara/certs" {
		t.Errorf("certCache is %q", cfg.certCache)
	}
	if cfg.ttl != 90*time.Second {
		t.Errorf("ttl is %s, want 1m30s", cfg.ttl)
	}
}

// TestParseConfigVersion checks that -version answers and stops, which has to
// work even when the rest of the command line would be rejected.
//
// Scenario: -version alone, then -version with a fatal flag combination.
// Expect: the version line both times, and no config to serve.
func TestParseConfigVersion(t *testing.T) {
	for _, name := range []string{"alone", "with an invalid ttl"} {
		t.Run(name, func(t *testing.T) {
			args := []string{"-version"}
			if name != "alone" {
				args = append(args, "-ttl", "0")
			}

			var out bytes.Buffer
			cfg, err := parseConfig(args, &out, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if cfg != nil {
				t.Fatal("got a config, want nothing left to serve")
			}
			if got := out.String(); !strings.HasPrefix(got, "filebadara "+version+" (") {
				t.Fatalf("printed %q, want it to start with the name and version", strings.TrimSpace(got))
			}
		})
	}
}

// TestParseConfigHelp checks that -h keeps behaving like a successful command
// rather than becoming a startup failure now that flag errors are returned.
//
// Scenario: -h is passed.
// Expect: usage on the writer, no error, and nothing to serve.
func TestParseConfigHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	cfg, err := parseConfig([]string{"-h"}, &out, &errOut)
	if err != nil {
		t.Fatalf("got %v, want -h to succeed", err)
	}
	if cfg != nil {
		t.Fatal("got a config, want nothing left to serve")
	}
	if !strings.Contains(errOut.String(), "-password-file") {
		t.Fatalf("usage does not list the flags:\n%s", errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("usage leaked onto stdout: %q", out.String())
	}
}

// TestParseConfigRejectsBadCombinations checks the rules that stop the server
// before it listens. Each of these would otherwise be a silent misconfiguration.
//
// Scenario: every rejected command line, including a password over plain HTTP.
// Expect: an error naming the problem, and no config.
func TestParseConfigRejectsBadCombinations(t *testing.T) {
	dir := t.TempDir()

	secret := filepath.Join(dir, "password")
	if err := os.WriteFile(secret, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readable := filepath.Join(dir, "readable")
	if err := os.WriteFile(readable, []byte("hunter2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name  string
		args  []string
		want  string
		usage bool // flag already reported it, so main must exit quietly
	}{
		{
			name: "password without a domain",
			args: []string{"-password-file", secret},
			want: "requires -domain",
		},
		{
			name: "password with an address but still no domain",
			args: []string{"-password-file", secret, "-addr", ":8080"},
			want: "requires -domain",
		},
		{
			name: "password file others can read",
			args: []string{"-password-file", readable, "-domain", "badara.example.com"},
			want: "group or others",
		},
		{
			name: "empty password file",
			args: []string{"-password-file", empty, "-domain", "badara.example.com"},
			want: "empty",
		},
		{
			name: "missing password file",
			args: []string{"-password-file", filepath.Join(dir, "absent"), "-domain", "badara.example.com"},
			want: "read password file",
		},
		{
			name: "zero ttl",
			args: []string{"-ttl", "0"},
			want: "-ttl must be greater than zero",
		},
		{
			name: "negative ttl",
			args: []string{"-ttl", "-5m"},
			want: "-ttl must be greater than zero",
		},
		{
			name:  "unknown flag",
			args:  []string{"-nope"},
			want:  "not defined",
			usage: true,
		},
		{
			name:  "unparsable ttl",
			args:  []string{"-ttl", "soon"},
			want:  "invalid value",
			usage: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseConfig(test.args, io.Discard, io.Discard)
			if err == nil {
				t.Fatalf("got config %+v, want an error", cfg)
			}
			if cfg != nil {
				t.Fatal("got both a config and an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error is %q, want it to mention %q", err, test.want)
			}
			if got := errors.Is(err, errUsage); got != test.usage {
				t.Fatalf("errors.Is(err, errUsage) is %v, want %v", got, test.usage)
			}
		})
	}
}

// TestParseConfigAcceptsPasswordWithDomain checks the one combination that is
// allowed to carry a password, so the rule above cannot become a blanket ban.
//
// Scenario: a 0600 password file together with -domain.
// Expect: the password is loaded and the trailing newline stripped.
func TestParseConfigAcceptsPasswordWithDomain(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(secret, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := parseConfig([]string{
		"-password-file", secret,
		"-domain", "badara.example.com",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.password != "hunter2" {
		t.Fatalf("password is %q, want hunter2", cfg.password)
	}
}
