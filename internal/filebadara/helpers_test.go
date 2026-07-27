package filebadara

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The helper scripts are shipped as text and only ever execute outside Go, so
// nothing else in this package can catch a shell or PowerShell mistake in them.
// These tests fetch the real script from a running server and run it.

// TestShellHelperEndToEnd checks that the /sh script a sender actually pipes
// into their shell works, from creating the sharing URL to serving a resume.
//
// Scenario: the served script is run against a real file, and the URL it prints
// is downloaded whole, then again, then partially.
// Expect: byte-identical results every time, and 206 for the range.
func TestShellHelperEndToEnd(t *testing.T) {
	requireCommands(t, "sh", "curl")
	runHelper(t, "/sh", "helper.sh", func(script, file string) *exec.Cmd {
		return exec.Command("sh", script, file)
	})
}

// TestPowerShellHelperEndToEnd is the Windows sender's path through the same
// flow.
//
// Scenario: the served /ps script is run under pwsh against a real file.
// Expect: the same results as the shell helper.
func TestPowerShellHelperEndToEnd(t *testing.T) {
	// The script calls curl.exe by name because on Windows plain `curl` is an
	// alias for Invoke-WebRequest. Off Windows, that name has to be on PATH.
	requireCommands(t, "pwsh", "curl.exe")
	runHelper(t, "/ps", "helper.ps1", func(script, file string) *exec.Cmd {
		return exec.Command("pwsh", "-NoProfile", "-File", script, file)
	})
}

func runHelper(t *testing.T, route, scriptName string, command func(script, file string) *exec.Cmd) {
	t.Helper()
	if testing.Short() && os.Getenv("FILEBADARA_REQUIRE_HELPERS") == "" {
		t.Skip("runs external processes")
	}

	payload := bytes.Repeat([]byte("FileBadara!"), 8192) // 90112 bytes
	dir := t.TempDir()
	file := filepath.Join(dir, "archive.bin")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(New("", "", time.Minute))
	// Registered before the helper's cleanup so it runs after it: cleanups are
	// LIFO. The helper parks a long-poll on /wait that only ends when the
	// transfer expires, so Close would otherwise block for the whole TTL.
	// Dropping the connections cancels that request's context immediately.
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})

	script := filepath.Join(dir, scriptName)
	if err := os.WriteFile(script, get(t, server.URL+route), 0o600); err != nil {
		t.Fatal(err)
	}

	downloadURL := startHelper(t, command(script, file))

	// The sender is given an absolute path, so this checks the published URL
	// carries only the base name. handleNew sanitises with path.Base, so this
	// holds even if a helper sends the whole path.
	if !strings.HasSuffix(downloadURL, "/archive.bin") {
		t.Fatalf("download URL %q does not end with the file's base name", downloadURL)
	}

	download(t, "whole file", downloadURL, "", http.StatusOK, payload)
	download(t, "same URL again", downloadURL, "", http.StatusOK, payload)
	download(t, "resume", downloadURL, "bytes=80000-", http.StatusPartialContent, payload[80000:])
}

// startHelper runs the sender script and returns the download URL it prints on
// its first line. The script keeps running until the test cleans it up, which
// is what makes the later downloads possible.
func startHelper(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	// A real file rather than an io.Writer. exec copies an io.Writer's stderr in
	// a goroutine, and Wait blocks until every process holding the write end has
	// closed it. The helper's curl grandchildren inherit that handle, so killing
	// the helper is not enough: on Windows this made cleanup sit out the whole
	// transfer lifetime.
	stderr, err := os.CreateTemp("", "filebadara-helper-stderr")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = stderr

	// Backstop in case a grandchild holds some other handle open.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stderr.Close()
		if out, err := os.ReadFile(stderr.Name()); err == nil && len(out) > 0 {
			t.Logf("helper stderr:\n%s", out)
		}
		// An orphaned grandchild may still hold it open on Windows.
		_ = os.Remove(stderr.Name())
	})

	// stderr is only read from the cleanup, which runs after Wait and so sees
	// everything the helper wrote.
	printed := make(chan string, 1)
	go func() {
		defer close(printed)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if line := strings.TrimSpace(scanner.Text()); line != "" {
				printed <- line
				return
			}
		}
	}()

	select {
	case line, ok := <-printed:
		if !ok {
			t.Fatal("helper exited without printing a URL")
		}
		return line
	case <-time.After(30 * time.Second):
		t.Fatal("helper printed no URL in time")
		return ""
	}
}

func download(t *testing.T, name, url, rangeHeader string, wantStatus int, want []byte) {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer response.Body.Close()

	if response.StatusCode != wantStatus {
		t.Fatalf("%s: got %d, want %d", name, response.StatusCode, wantStatus)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(body) != len(want) {
		t.Fatalf("%s: got %d bytes, want %d", name, len(body), len(want))
	}
	// A sender that ignores the offset returns the right length from the wrong
	// place, so comparing lengths alone would let that through.
	for i := range body {
		if body[i] != want[i] {
			t.Fatalf("%s: %d bytes returned but they differ from byte %d", name, len(body), i)
		}
	}
}

func get(t *testing.T, url string) []byte {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %d", url, response.StatusCode)
	}
	return body
}

// requireCommands skips when a helper's interpreter is missing, which is the
// right default on a developer machine. CI sets FILEBADARA_REQUIRE_HELPERS so a
// missing interpreter fails instead: a silent skip there would mean the helper
// scripts ship untested.
func requireCommands(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			if os.Getenv("FILEBADARA_REQUIRE_HELPERS") != "" {
				t.Fatalf("%s is not on PATH and FILEBADARA_REQUIRE_HELPERS is set", name)
			}
			t.Skipf("%s is not installed", name)
		}
	}
}
