package filebadara

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTwoConcurrentDownloadsUseIndependentUploads checks the core promise that
// one stream is never shared between downloaders.
//
// Scenario: two downloaders GET the same sharing URL at once, and the sender
// answers each of them with its own upload.
// Expect: both receive the complete file and both jobs start at offset 0, so
// neither download was fed from the other's stream.
func TestTwoConcurrentDownloadsUseIndependentUploads(t *testing.T) {
	app := New("", "", time.Minute)
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()

	payload := bytes.Repeat([]byte("FileBadara!"), 64*1024)
	downloadURL, waitURL := createTransfer(t, httpServer.URL, "test file.bin", len(payload), "")

	results := make(chan []byte, 2)
	errors := make(chan error, 2)
	for range 2 {
		go func() {
			response, err := http.Get(downloadURL)
			if err != nil {
				errors <- err
				return
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				errors <- err
				return
			}
			results <- body
		}()
	}

	var uploads sync.WaitGroup
	for range 2 {
		response, err := http.Get(waitURL)
		if err != nil {
			t.Fatal(err)
		}
		jobBytes, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		uploadURL, offset := parseJob(t, jobBytes)
		if offset != 0 {
			t.Fatalf("got offset %d, want 0 for a whole-file download", offset)
		}

		uploads.Add(1)
		go func() {
			defer uploads.Done()
			request, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(payload))
			if err != nil {
				errors <- err
				return
			}
			request.ContentLength = int64(len(payload))
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				errors <- err
				return
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				errors <- &statusError{response.StatusCode}
			}
		}()
	}

	for range 2 {
		select {
		case err := <-errors:
			t.Fatal(err)
		case body := <-results:
			if !bytes.Equal(body, payload) {
				t.Fatal("download differs from uploaded file")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("download timed out")
		}
	}

	uploads.Wait()
}

// TestUploadPassword checks that the password gates the creation of a sharing
// URL, which is the only action it is meant to protect.
//
// Scenario: a password-protected server receives POST /new twice, first without
// credentials, then with the correct Basic auth.
// Expect: 401 followed by 200.
func TestUploadPassword(t *testing.T) {
	app := New("", "correct horse", time.Minute)
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()

	form := url.Values{"name": {"file.txt"}, "size": {"11"}}

	response, err := http.PostForm(httpServer.URL+"/new", form)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", response.StatusCode)
	}

	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/new", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.SetBasicAuth("upload", "correct horse")

	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", response.StatusCode)
	}
}

// TestHelpersAndHome checks that the informational routes stay open even when a
// password is configured, since the sender has to fetch the helper before it can
// authenticate with it.
//
// Scenario: GET /, /sh and /ps on a password-protected server.
// Expect: all three return 200, and the home page names the project.
func TestHelpersAndHome(t *testing.T) {
	app := New("", "secret", time.Minute)
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()

	for _, path := range []string{"/", "/sh", "/ps"} {
		response, err := http.Get(httpServer.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s returned %d", path, response.StatusCode)
		}
		if !bytes.Contains(body, []byte("FileBadara")) && path == "/" {
			t.Fatal("home page does not contain project name")
		}
	}
}

// TestHSTSOnlyInHTTPSMode checks that the downgrade guard is sent exactly where
// it means something. Advertising it over plain HTTP would be a false promise.
//
// Scenario: the same request against a server built with a domain (HTTPS mode)
// and one built without it (plain HTTP mode).
// Expect: Strict-Transport-Security only in HTTPS mode.
func TestHSTSOnlyInHTTPSMode(t *testing.T) {
	for _, test := range []struct {
		name   string
		domain string
		want   string
	}{
		{"https mode", "drop.example.com", "max-age=31536000"},
		{"plain http mode", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			httpServer := httptest.NewServer(New(test.domain, "", time.Minute))
			defer httpServer.Close()

			response, err := http.Get(httpServer.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if got := response.Header.Get("Strict-Transport-Security"); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

// TestTransferExpires checks that the TTL actually retires a sharing URL rather
// than merely marking it stale.
//
// Scenario: a sharing URL is created with a 30ms lifetime, then requested once
// it has lapsed.
// Expect: 404, because the token is gone from the transfer table.
func TestTransferExpires(t *testing.T) {
	app := New("", "", 30*time.Millisecond)
	httpServer := httptest.NewServer(app)
	defer httpServer.Close()

	downloadURL, _ := createTransfer(t, httpServer.URL, "file.txt", 11, "")
	time.Sleep(80 * time.Millisecond)

	response, err := http.Get(downloadURL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", response.StatusCode)
	}
}

// TestParseByteRange checks the header parser in isolation, including the shapes
// a real client sends and the ones an attacker or a broken client might.
//
// Scenario: every Range header form is parsed against a 1000-byte resource.
// Expect: a usable range for valid input; errRangeNotSatisfiable for a range
// past the end; and nil for anything unparsable or multi-range, which tells the
// caller to serve the whole body.
func TestParseByteRange(t *testing.T) {
	const size = 1000
	for _, test := range []struct {
		header string
		start  int64
		length int64
		whole  bool
		bad    bool
	}{
		{header: "", whole: true},
		{header: "bytes=0-99", start: 0, length: 100},
		{header: "bytes=100-199", start: 100, length: 100},
		{header: "bytes=100-", start: 100, length: 900},
		{header: "bytes=0-", start: 0, length: 1000},
		{header: "bytes=999-", start: 999, length: 1},
		{header: "bytes=-500", start: 500, length: 500},
		{header: "bytes=-5000", start: 0, length: 1000}, // clamped to the whole body
		{header: "bytes=100-99999", start: 100, length: 900},
		{header: " bytes=100-199 ", start: 100, length: 100},
		{header: "bytes=100 - 199", start: 100, length: 100},
		{header: "bytes=1000-", bad: true},
		{header: "bytes=5000-6000", bad: true},
		{header: "bytes=-0", bad: true},
		{header: "bytes=0-99,200-299", whole: true}, // multi-range is served whole
		{header: "items=0-99", whole: true},
		{header: "bytes=abc-def", whole: true},
		{header: "bytes=200-100", whole: true},
		{header: "bytes=-abc", whole: true},
		{header: "bytes=100", whole: true},
		{header: "bytes = 100-199", whole: true}, // whitespace around "=" is not accepted, matching net/http
	} {
		t.Run(test.header, func(t *testing.T) {
			got, err := parseByteRange(test.header, size)
			switch {
			case test.bad:
				if !errors.Is(err, errRangeNotSatisfiable) {
					t.Fatalf("got (%v, %v), want errRangeNotSatisfiable", got, err)
				}
			case test.whole:
				if err != nil || got != nil {
					t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if got == nil {
					t.Fatalf("got no range, want start=%d length=%d", test.start, test.length)
				}
				if got.start != test.start || got.length != test.length {
					t.Fatalf("got %+v, want start=%d length=%d", got, test.start, test.length)
				}
			}
		})
	}
}

// TestParseByteRangeOnEmptyResource pins down the zero-length case, where the
// arithmetic in the parser has no valid byte position to land on.
//
// Scenario: an empty file is asked for its first byte and for its last byte.
// Expect: both are unsatisfiable rather than an empty 206.
func TestParseByteRangeOnEmptyResource(t *testing.T) {
	for _, header := range []string{"bytes=0-", "bytes=-1"} {
		if _, err := parseByteRange(header, 0); !errors.Is(err, errRangeNotSatisfiable) {
			t.Fatalf("%s on a zero-length body: got %v, want errRangeNotSatisfiable", header, err)
		}
	}
}

// TestRangeDownload checks each Range shape over a real HTTP round trip, and
// guards the reason ranges were added at all: the sender must not pay for bytes
// the downloader already has.
//
// Scenario: a downloader issues each Range shape while a stub sender answers
// the job from the offset it was given.
// Expect: the documented status, headers and bytes, plus a sender upload cost
// matching senderUploads.
func TestRangeDownload(t *testing.T) {
	payload := bytes.Repeat([]byte("FileBadara!"), 4096) // 45056 bytes
	// senderUploads is what the sender pays. The sender seeks to the offset
	// but always runs to the end of the file, so only a bounded range leaves
	// a tail for the server to cut off.
	for _, test := range []struct {
		name          string
		header        string
		status        int
		contentType   string
		want          []byte
		senderUploads int64
	}{
		{
			name:          "middle",
			header:        "bytes=1000-1999",
			status:        http.StatusPartialContent,
			contentType:   fmt.Sprintf("bytes 1000-1999/%d", len(payload)),
			want:          payload[1000:2000],
			senderUploads: 45056 - 1000, // seeks past the prefix, tail is cut off here
		},
		{
			name:          "open ended",
			header:        "bytes=40000-",
			status:        http.StatusPartialContent,
			contentType:   fmt.Sprintf("bytes 40000-%d/%d", len(payload)-1, len(payload)),
			want:          payload[40000:],
			senderUploads: 5056, // exactly the slice, nothing wasted
		},
		{
			name:          "suffix",
			header:        "bytes=-500",
			status:        http.StatusPartialContent,
			contentType:   fmt.Sprintf("bytes %d-%d/%d", len(payload)-500, len(payload)-1, len(payload)),
			want:          payload[len(payload)-500:],
			senderUploads: 500, // exactly the slice
		},
		{
			name:          "no range header",
			status:        http.StatusOK,
			want:          payload,
			senderUploads: 45056,
		},
		{
			name:          "multi range falls back to the whole body",
			header:        "bytes=0-99,200-299",
			status:        http.StatusOK,
			want:          payload,
			senderUploads: 45056,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			httpServer := httptest.NewServer(New("", "", time.Minute))
			defer httpServer.Close()

			downloadURL, waitURL := createTransfer(t, httpServer.URL, "file.bin", len(payload), "")
			uploaded := make(chan int64, 1)
			go servePayload(t, waitURL, payload, uploaded)

			request, err := http.NewRequest(http.MethodGet, downloadURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.header != "" {
				request.Header.Set("Range", test.header)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()

			if response.StatusCode != test.status {
				t.Fatalf("got %d, want %d", response.StatusCode, test.status)
			}
			if got := response.Header.Get("Accept-Ranges"); got != "bytes" {
				t.Fatalf("Accept-Ranges is %q, want bytes", got)
			}
			if got := response.Header.Get("Content-Range"); got != test.contentType {
				t.Fatalf("Content-Range is %q, want %q", got, test.contentType)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, test.want) {
				t.Fatalf("got %d bytes, want %d", len(body), len(test.want))
			}
			if response.ContentLength != int64(len(test.want)) {
				t.Fatalf("Content-Length is %d, want %d", response.ContentLength, len(test.want))
			}
			select {
			case got := <-uploaded:
				if got != test.senderUploads {
					t.Fatalf("sender uploaded %d bytes, want %d", got, test.senderUploads)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("sender never uploaded")
			}
		})
	}
}

// TestRangeNotSatisfiable checks the payoff of learning the file size at /new
// time: a hopeless request is turned away for free.
//
// Scenario: a range past the end of the file is requested, with no sender
// running at all.
// Expect: 416 with Content-Range: bytes */SIZE, which is only reachable if the
// server rejected the range without waiting for an upload.
func TestRangeNotSatisfiable(t *testing.T) {
	payload := []byte("FileBadara!")
	httpServer := httptest.NewServer(New("", "", time.Minute))
	defer httpServer.Close()

	// No sender is started: because the size is known from /new, an impossible
	// range is rejected without ever waking the sender.
	downloadURL, _ := createTransfer(t, httpServer.URL, "file.bin", len(payload), "")

	request, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=9999-")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("got %d, want 416", response.StatusCode)
	}
	if got, want := response.Header.Get("Content-Range"), fmt.Sprintf("bytes */%d", len(payload)); got != want {
		t.Fatalf("Content-Range is %q, want %q", got, want)
	}
}

// parseJob splits a /wait response into the upload URL and the offset the
// sender is expected to start from.
func parseJob(t *testing.T, body []byte) (string, int64) {
	t.Helper()
	fields := strings.Fields(string(body))
	if len(fields) != 2 {
		t.Fatalf("unexpected job response: %q", body)
	}
	offset, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("bad offset in %q: %v", body, err)
	}
	return fields[0], offset
}

// servePayload plays the role of the sender helper for exactly one download.
// Like `curl -C`, it uploads from the requested offset to the end of the file
// and records how many bytes that cost. Sending the whole payload regardless
// would hide the very waste these tests exist to catch.
func servePayload(t *testing.T, waitURL string, payload []byte, uploaded chan<- int64) {
	response, err := http.Get(waitURL)
	if err != nil {
		return
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return
	}
	uploadURL, offset := parseJob(t, body)

	slice := payload[offset:]
	if uploaded != nil {
		uploaded <- int64(len(slice))
	}

	request, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(slice))
	if err != nil {
		return
	}
	request.ContentLength = int64(len(slice))
	if response, err := http.DefaultClient.Do(request); err == nil {
		response.Body.Close()
	}
}

// createTransfer posts to /new the way a helper script does and returns the
// download URL and the wait URL.
func createTransfer(t *testing.T, base, filename string, size int, password string) (string, string) {
	t.Helper()
	form := url.Values{"name": {filename}, "size": {strconv.Itoa(size)}}
	request, err := http.NewRequest(http.MethodPost, base+"/new", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if password != "" {
		request.SetBasicAuth("upload", password)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("create transfer returned %d: %s", response.StatusCode, body)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected response: %q", body)
	}
	return lines[0], lines[1]
}

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }
