package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pjknkda/filebadara/internal/filebadara"
	"golang.org/x/crypto/acme/autocert"
)

// version is set by the release build through -ldflags -X.
var version = "dev"

// config is a command line that has already been checked for contradictions.
type config struct {
	addr      string
	domain    string
	password  string
	certCache string
	ttl       time.Duration
}

// errUsage reports a command line the flag package has already complained
// about, in its own words and with the usage text. Reporting it again would
// print the same problem twice.
var errUsage = errors.New("invalid command line")

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case errors.Is(err, errUsage):
		os.Exit(2) // what flag.ExitOnError would have used
	case err != nil:
		log.Fatal(err)
	case cfg == nil:
		return // -version and -h answer on their own
	}
	log.Fatal(serve(cfg))
}

// parseConfig turns arguments into a validated config. It returns a nil config
// and a nil error when the arguments asked for something the command has
// already done, such as printing the version. Errors are returned rather than
// exiting so that the rules below can be tested.
//
// out carries output that was asked for, such as the version; errOut carries
// the flag package's complaints and usage text.
func parseConfig(args []string, out, errOut io.Writer) (*config, error) {
	var cfg config
	var passwordFile string
	var showVersion bool

	flags := flag.NewFlagSet("filebadara", flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.StringVar(&cfg.addr, "addr", ":80", "HTTP listen address when -domain is empty")
	flags.StringVar(&cfg.domain, "domain", "", "domain name; enables automatic HTTPS on ports 80 and 443")
	flags.StringVar(&passwordFile, "password-file", "", "file containing the upload password")
	flags.StringVar(&cfg.certCache, "cert-cache", ".filebadara-certs", "ACME certificate cache directory")
	flags.DurationVar(&cfg.ttl, "ttl", filebadara.DefaultTransferTTL, "lifetime of a sharing URL")
	flags.BoolVar(&showVersion, "version", false, "print the version and exit")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %w", errUsage, err)
	}

	// Answered before anything else is validated, so it always works.
	if showVersion {
		fmt.Fprintf(out, "filebadara %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil, nil
	}

	cfg.domain = strings.TrimSpace(cfg.domain)

	password, err := filebadara.ReadPasswordFile(passwordFile)
	if err != nil {
		return nil, err
	}
	cfg.password = password

	if cfg.password != "" && cfg.domain == "" {
		return nil, errors.New("-password-file requires -domain because passwords must use HTTPS")
	}
	if cfg.ttl <= 0 {
		return nil, errors.New("-ttl must be greater than zero")
	}
	return &cfg, nil
}

// serve runs until the listener fails, so it only ever returns an error.
func serve(cfg *config) error {
	app := filebadara.New(cfg.domain, cfg.password, cfg.ttl)

	if cfg.domain == "" {
		log.Printf("FileBadara listening on http://%s", cfg.addr)
		return http.ListenAndServe(cfg.addr, app)
	}

	manager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(cfg.certCache),
		HostPolicy: autocert.HostWhitelist(cfg.domain),
	}

	go func() {
		log.Printf("ACME challenge and HTTP redirect server listening on :80")
		if err := http.ListenAndServe(":80", manager.HTTPHandler(nil)); err != nil {
			log.Printf("HTTP server stopped: %v", err)
			os.Exit(1)
		}
	}()

	server := &http.Server{
		Addr:              ":443",
		Handler:           app,
		TLSConfig:         manager.TLSConfig(),
		ReadHeaderTimeout: filebadara.ReadHeaderTimeout,
	}

	log.Printf("FileBadara listening on https://%s", cfg.domain)
	if err := server.ListenAndServeTLS("", ""); err != nil {
		return fmt.Errorf("HTTPS server stopped: %w", err)
	}
	return nil
}
