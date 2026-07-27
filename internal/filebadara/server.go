package filebadara

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTransferTTL is the sharing URL lifetime used when none is given.
	DefaultTransferTTL = 10 * time.Minute
	ReadHeaderTimeout  = 10 * time.Second
	senderWaitTimeout  = 30 * time.Second
)

// A job is one downloader waiting for the sender to stream the file again.
// start is the offset the sender must begin at, which lets a range request
// skip the prefix on the sender's side instead of discarding it here.
type job struct {
	id    string
	start int64

	attached sync.Once
	finished sync.Once
	upload   chan io.ReadCloser
	done     chan struct{}
}

func newJob(start int64) *job {
	return &job{
		id:     randomToken(18),
		start:  start,
		upload: make(chan io.ReadCloser, 1),
		done:   make(chan struct{}),
	}
}

func (j *job) attach(body io.ReadCloser) bool {
	attached := false
	j.attached.Do(func() {
		j.upload <- body
		attached = true
	})
	return attached
}

func (j *job) finish() {
	j.finished.Do(func() { close(j.done) })
}

type transfer struct {
	filename string
	size     int64
	secret   string
	jobs     chan *job
	expired  chan struct{}

	mu      sync.Mutex
	pending map[string]*job
}

// Server implements the complete FileBadara HTTP API.
type Server struct {
	domain   string
	password string
	ttl      time.Duration

	mu        sync.RWMutex
	transfers map[string]*transfer
}

// New creates a FileBadara server. An empty domain means plain HTTP mode.
// ttl is the lifetime of every sharing URL the server hands out.
func New(domain, password string, ttl time.Duration) *Server {
	return &Server{
		domain:    domain,
		password:  password,
		ttl:       ttl,
		transfers: make(map[string]*transfer),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.domain != "" {
		// Keep clients from ever retrying over plain HTTP, where the upload
		// password would travel in the clear.
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}

	switch r.URL.Path {
	case "/":
		s.handleHome(w, r)
	case "/sh":
		s.handleShell(w, r)
	case "/ps":
		s.handlePowerShell(w, r)
	case "/new":
		s.handleNew(w, r)
	default:
		switch {
		case strings.HasPrefix(r.URL.Path, "/wait/"):
			s.handleWait(w, r)
		case strings.HasPrefix(r.URL.Path, "/upload/"):
			s.handleUpload(w, r)
		default:
			s.handleDownload(w, r)
		}
	}
}

func (s *Server) baseURL(r *http.Request) string {
	if s.domain != "" {
		return "https://" + s.domain
	}
	return "http://" + r.Host
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	base := s.baseURL(r)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, `FileBadara

Unix:
  curl -fsSL %s/sh | sh -s -- FILE

PowerShell:
  & ([scriptblock]::Create((irm %s/ps))) FILE

The sender command stays open while the URL is available.
Each download starts a separate upload stream from the sender.
`, base, base)
}

func (s *Server) handleNew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="FileBadara upload"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	filename := strings.TrimSpace(r.Form.Get("name"))
	filename = path.Base(filename)
	if filename == "" || filename == "." || filename == "/" {
		http.Error(w, "missing filename", http.StatusBadRequest)
		return
	}

	// The size is what lets the server resolve a Range before it asks the
	// sender for anything.
	size, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("size")), 10, 64)
	if err != nil || size < 0 {
		http.Error(w, "missing or invalid size", http.StatusBadRequest)
		return
	}

	token := randomToken(18)
	secret := randomToken(24)
	t := &transfer{
		filename: filename,
		size:     size,
		secret:   secret,
		jobs:     make(chan *job),
		expired:  make(chan struct{}),
		pending:  make(map[string]*job),
	}

	s.mu.Lock()
	s.transfers[token] = t
	s.mu.Unlock()

	go func() {
		timer := time.NewTimer(s.ttl)
		defer timer.Stop()
		<-timer.C
		close(t.expired)
		s.mu.Lock()
		delete(s.transfers, token)
		s.mu.Unlock()
	}()

	base := s.baseURL(r)
	downloadURL := base + "/" + token + "/" + url.PathEscape(filename)
	waitURL := base + "/wait/" + token + "/" + secret

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%s\n%s\n", downloadURL, waitURL)
}

func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	parts := splitPath(r.URL.Path)
	if len(parts) != 3 || parts[0] != "wait" {
		http.NotFound(w, r)
		return
	}

	t := s.getTransfer(parts[1])
	if t == nil || !same(parts[2], t.secret) {
		http.NotFound(w, r)
		return
	}

	select {
	case j := <-t.jobs:
		// The trailing offset tells the sender where to start reading.
		fmt.Fprintf(w, "%s/upload/%s/%s/%s %d\n", s.baseURL(r), parts[1], t.secret, j.id, j.start)
	case <-t.expired:
		http.Error(w, "expired", http.StatusGone)
	case <-r.Context().Done():
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	parts := splitPath(r.URL.Path)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}

	token := parts[0]
	t := s.getTransfer(token)
	if t == nil {
		http.NotFound(w, r)
		return
	}

	// Resolving the range up front means an impossible one costs the sender
	// nothing at all.
	w.Header().Set("Accept-Ranges", "bytes")
	chosen, err := parseByteRange(r.Header.Get("Range"), t.size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", t.size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	partial := chosen != nil
	if !partial {
		chosen = &byteRange{start: 0, length: t.size}
	}

	j := newJob(chosen.start)

	t.mu.Lock()
	t.pending[j.id] = j
	t.mu.Unlock()

	defer func() {
		j.finish()
		t.mu.Lock()
		delete(t.pending, j.id)
		t.mu.Unlock()
	}()

	select {
	case t.jobs <- j:
	case <-t.expired:
		http.Error(w, "expired", http.StatusGone)
		return
	case <-r.Context().Done():
		return
	}

	var body io.ReadCloser
	select {
	case body = <-j.upload:
	case <-r.Context().Done():
		return
	case <-time.After(senderWaitTimeout):
		http.Error(w, "sender unavailable", http.StatusGatewayTimeout)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", chosen.length))
	if value := mime.FormatMediaType("attachment", map[string]string{"filename": t.filename}); value != "" {
		w.Header().Set("Content-Disposition", value)
	}
	if partial {
		last := chosen.start + chosen.length - 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", chosen.start, last, t.size))
		w.WriteHeader(http.StatusPartialContent)
	}

	// The sender streams from chosen.start to the end of the file, so anything
	// past the requested slice is cut off here.
	_, _ = io.CopyN(w, body, chosen.length)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w)
		return
	}

	parts := splitPath(r.URL.Path)
	if len(parts) != 4 || parts[0] != "upload" {
		http.NotFound(w, r)
		return
	}

	t := s.getTransfer(parts[1])
	if t == nil || !same(parts[2], t.secret) {
		http.NotFound(w, r)
		return
	}

	t.mu.Lock()
	j := t.pending[parts[3]]
	t.mu.Unlock()
	if j == nil {
		http.Error(w, "download request is gone", http.StatusGone)
		return
	}

	if !j.attach(r.Body) {
		http.Error(w, "upload already attached", http.StatusConflict)
		return
	}

	select {
	case <-j.done:
		w.WriteHeader(http.StatusOK)
	case <-r.Context().Done():
		j.finish()
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.password == "" {
		return true
	}
	username, password, ok := r.BasicAuth()
	return ok && same(username, "upload") && same(password, s.password)
}

func (s *Server) getTransfer(token string) *transfer {
	s.mu.RLock()
	t := s.transfers[token]
	s.mu.RUnlock()
	return t
}

// errRangeNotSatisfiable reports a syntactically valid range that falls outside
// the resource, which must be answered with 416 rather than the whole body.
var errRangeNotSatisfiable = errors.New("range not satisfiable")

type byteRange struct {
	start  int64
	length int64
}

// parseByteRange interprets a Range header against a known total size. A nil
// range means the caller should serve the entire body, which RFC 9110 allows
// whenever the header is absent, unparsable, or asks for several ranges.
func parseByteRange(header string, size int64) (*byteRange, error) {
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return nil, nil
	}
	first, last, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return nil, nil
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)

	// "bytes=-N" asks for the final N bytes.
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return nil, nil
		}
		if n == 0 || size == 0 {
			return nil, errRangeNotSatisfiable
		}
		if n > size {
			n = size
		}
		return &byteRange{start: size - n, length: n}, nil
	}

	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return nil, nil
	}
	if start >= size {
		return nil, errRangeNotSatisfiable
	}

	end := size - 1
	if last != "" {
		if end, err = strconv.ParseInt(last, 10, 64); err != nil || end < start {
			return nil, nil
		}
		if end > size-1 {
			end = size - 1
		}
	}
	return &byteRange{start: start, length: end - start + 1}, nil
}

func splitPath(value string) []string {
	value = strings.Trim(value, "/")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

func randomToken(size int) string {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func same(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
