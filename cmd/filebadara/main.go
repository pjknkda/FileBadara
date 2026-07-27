package main

import (
	"flag"
	"fmt"
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

func main() {
	var addr string
	var domain string
	var passwordFile string
	var certCache string
	var ttl time.Duration
	var showVersion bool

	flag.StringVar(&addr, "addr", ":80", "HTTP listen address when -domain is empty")
	flag.StringVar(&domain, "domain", "", "domain name; enables automatic HTTPS on ports 80 and 443")
	flag.StringVar(&passwordFile, "password-file", "", "file containing the upload password")
	flag.StringVar(&certCache, "cert-cache", ".filebadara-certs", "ACME certificate cache directory")
	flag.DurationVar(&ttl, "ttl", filebadara.DefaultTransferTTL, "lifetime of a sharing URL")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.Parse()

	// Answered before any other flag is validated, so it always works.
	if showVersion {
		fmt.Printf("filebadara %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	domain = strings.TrimSpace(domain)
	password, err := filebadara.ReadPasswordFile(passwordFile)
	if err != nil {
		log.Fatal(err)
	}
	if password != "" && domain == "" {
		log.Fatal("-password-file requires -domain because passwords must use HTTPS")
	}
	if ttl <= 0 {
		log.Fatal("-ttl must be greater than zero")
	}

	app := filebadara.New(domain, password, ttl)

	if domain == "" {
		log.Printf("FileBadara listening on http://%s", addr)
		log.Fatal(http.ListenAndServe(addr, app))
	}

	manager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(certCache),
		HostPolicy: autocert.HostWhitelist(domain),
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

	log.Printf("FileBadara listening on https://%s", domain)
	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatal(fmt.Errorf("HTTPS server stopped: %w", err))
	}
}
