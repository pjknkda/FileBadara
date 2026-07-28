package filebadara

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
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
	// client is the downloader's address, passed on to the sender so both ends
	// of a transfer can log who it was for.
	client string

	attached sync.Once
	finished sync.Once
	upload   chan io.ReadCloser
	done     chan struct{}
}

func newJob(start int64, client string) *job {
	return &job{
		id:     randomToken(18),
		start:  start,
		client: client,
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

	// Logger receives one line per download. Set it before serving; nil uses
	// the standard logger.
	Logger *log.Logger

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

	s.logf("share token=%s file=%q size=%d client=%s ttl=%s",
		shortToken(token), filename, size, clientHost(r), s.ttl)

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

	// This long-poll is the sender's connection to the server: it stays open
	// until there is something to send, so it doubles as a liveness signal.
	token := shortToken(parts[1])
	client := clientHost(r)
	s.logf("sender token=%s client=%s status=waiting", token, client)

	select {
	case j := <-t.jobs:
		// The offset tells the sender where to start reading, and the address
		// after it is the downloader the bytes are for. Both are whitespace-free
		// so the helper scripts can split the line.
		fmt.Fprintf(w, "%s/upload/%s/%s/%s %d %s\n", s.baseURL(r), parts[1], t.secret, j.id, j.start, j.client)
		s.logf("sender token=%s client=%s job=%s offset=%d downloader=%s status=dispatched",
			token, client, shortToken(j.id), j.start, j.client)
	case <-t.expired:
		http.Error(w, "expired", http.StatusGone)
		s.logf("sender token=%s client=%s status=expired", token, client)
	case <-r.Context().Done():
		s.logf("sender token=%s client=%s status=disconnected", token, client)
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

	started := time.Now()
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

	served := "full"
	if partial {
		served = fmt.Sprintf("%d-%d", chosen.start, chosen.start+chosen.length-1)
	}
	s.logf("download token=%s file=%q client=%s agent=%q range=%s status=start",
		shortToken(token), t.filename, clientHost(r), userAgent(r), served)

	j := newJob(chosen.start, clientHost(r))

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
		s.logf("download file=%q client=%s token=%s status=%q",
			t.filename, clientHost(r), shortToken(token), "sender unavailable")
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
	sent, err := io.CopyN(w, body, chosen.length)

	status := "ok"
	if err != nil {
		status = err.Error()
	}
	s.logf("download token=%s file=%q client=%s range=%s sent=%d/%d took=%s status=%q",
		shortToken(token), t.filename, clientHost(r), served,
		sent, chosen.length, time.Since(started).Round(time.Millisecond), status)
}

func (s *Server) logf(format string, args ...any) {
	logger := s.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf(format, args...)
}

// shortToken keeps enough of a token to tie log lines about one sharing URL
// together. Logging the whole thing would let anyone reading the log download
// the file for as long as the URL lives.
func shortToken(token string) string {
	const keep = 8
	if len(token) <= keep {
		return token
	}
	return token[:keep]
}

// userAgent trims the header to something a log line can hold. The caller must
// print it with %q: it is attacker-controlled, and a newline in it would
// otherwise let a downloader forge log entries.
func userAgent(r *http.Request) string {
	const limit = 120
	agent := r.UserAgent()
	if len(agent) > limit {
		return agent[:limit] + "..."
	}
	return agent
}

// clientHost never returns an empty string: the /wait response puts it in a
// whitespace-separated field, so a missing address has to still be one token.
func clientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if strings.TrimSpace(host) == "" {
		return "unknown"
	}
	return host
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

	token := shortToken(parts[1])
	client := clientHost(r)
	job := shortToken(parts[3])

	t.mu.Lock()
	j := t.pending[parts[3]]
	t.mu.Unlock()
	if j == nil {
		http.Error(w, "download request is gone", http.StatusGone)
		s.logf("upload token=%s client=%s job=%s status=gone", token, client, job)
		return
	}

	if !j.attach(r.Body) {
		http.Error(w, "upload already attached", http.StatusConflict)
		s.logf("upload token=%s client=%s job=%s status=duplicate", token, client, job)
		return
	}

	started := time.Now()
	s.logf("upload token=%s client=%s job=%s offset=%d status=start", token, client, job, j.start)

	status := "done"
	select {
	case <-j.done:
		w.WriteHeader(http.StatusOK)
	case <-r.Context().Done():
		j.finish()
		status = "disconnected"
	}
	s.logf("upload token=%s client=%s job=%s took=%s status=%s",
		token, client, job, time.Since(started).Round(time.Millisecond), status)
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
