// Command tcdi-xferd is the in-pod file-transfer helper (see internal/xferd).
// It reads everything from the pod: the transfer credential from the
// mounted Secret, the TLS pair the entrypoint staged on /run/tcdi, and the
// policy floor from the operator-rendered env.
package main

import (
	"crypto/tls"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tinyorbitvn/tinycdi/internal/xferd"
)

func main() {
	secretDir := envOr("TCDI_SECRET_DIR", "/run/secrets/tcdi")
	rt := envOr("TCDI_RUNTIME_DIR", "/run/tcdi")
	home := envOr("HOME", "/home/workspace")
	token, err := os.ReadFile(filepath.Join(secretDir, "xfer-token"))
	if err != nil {
		// Every workspace Secret is created with the key; a missing one is a
		// broken pod, not a mode. Exit so the supervisor restarts the pod.
		log.Fatalf("tcdi-xferd: transfer credential: %v", err)
	}
	tok := strings.TrimRight(string(token), "\r\n")
	if tok == "" {
		log.Fatal("tcdi-xferd: transfer credential is empty")
	}
	max, err := strconv.ParseInt(os.Getenv("TCDI_XFER_MAX_BYTES"), 10, 64)
	if err != nil || max < 0 {
		max = 0
	}
	certPEM, err := os.ReadFile(filepath.Join(rt, "tls.crt"))
	if err != nil {
		log.Fatalf("tcdi-xferd: staged certificate: %v", err)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(rt, "tls.crt"), filepath.Join(rt, "tls.key"))
	if err != nil {
		log.Fatalf("tcdi-xferd: staged key pair: %v", err)
	}
	up, _ := url.Parse("https://127.0.0.1:" + envOr("TCDI_XFER_UPSTREAM_PORT", "8444"))
	h, err := xferd.New(xferd.Config{
		Root: filepath.Join(home, "transfers"), Token: []byte(tok),
		MaxBytes: max, Policy: os.Getenv("TCDI_XFER_POLICY"),
		Upstream: up, UpstreamServerName: "localhost", UpstreamCAPEM: certPEM,
	})
	if err != nil {
		log.Fatalf("tcdi-xferd: %v", err)
	}
	srv := &http.Server{
		Addr:              ":8443",
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
	}
	log.Printf("tcdi-xferd: serving :8443 (policy %q, cap %d bytes)", os.Getenv("TCDI_XFER_POLICY"), max)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
