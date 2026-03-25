// mpquic-mgmt — Management REST API daemon for MPQUIC tunnel instances.
//
// Provides unified control plane for:
//   - Instance discovery and lifecycle (start/stop/restart)
//   - Configuration CRUD with validation and parameter classification
//   - Metrics aggregation proxy
//   - System operations (logs, update, info)
//
// Designed to run on the TBOX client alongside tunnel instances.
// Consumed by LuCI UI (OpenWrt) and AI/ML decision layer.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev" // set via -ldflags at build time

func main() {
	listenAddr := flag.String("listen", "127.0.0.1:8080", "HTTP listen address (default: localhost only)")
	instanceDir := flag.String("instance-dir", "/etc/mpquic/instances", "directory containing tunnel YAML configs")
	authToken := flag.String("auth-token", "", "Bearer token for API auth (prefer MGMT_AUTH_TOKEN env)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file (enables HTTPS)")
	tlsKey := flag.String("tls-key", "", "TLS private key file")
	allowedOrigins := flag.String("cors-origins", "", "Comma-separated allowed CORS origins (empty = none)")
	flag.Parse()

	// Auth token: prefer env var (avoids /proc/PID/cmdline leak)
	token := os.Getenv("MGMT_AUTH_TOKEN")
	if token == "" {
		token = *authToken
	}
	if token == "" {
		log.Fatal("FATAL: auth token required. Set MGMT_AUTH_TOKEN env var or --auth-token flag")
	}
	if len(token) < 16 {
		log.Fatal("FATAL: auth token too short (minimum 16 characters)")
	}

	mgr := NewInstanceManager(*instanceDir)
	h := NewAPIHandler(mgr, token, *allowedOrigins)

	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("/api/v1/health", h.HandleHealth)

	// Tunnels
	mux.HandleFunc("/api/v1/tunnels", h.HandleTunnels)
	// Tunnel-specific routes: /api/v1/tunnels/{name}, /api/v1/tunnels/{name}/start, etc.
	mux.HandleFunc("/api/v1/tunnels/", h.HandleTunnelRoutes)

	// Metrics
	mux.HandleFunc("/api/v1/metrics", h.HandleMetricsAll)

	// System
	mux.HandleFunc("/api/v1/system/info", h.HandleSystemInfo)
	mux.HandleFunc("/api/v1/system/logs/", h.HandleSystemLogs)

	server := &http.Server{
		Addr:         *listenAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if *tlsCert != "" && *tlsKey != "" {
		server.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			},
		}
		log.Printf("mpquic-mgmt %s listening on %s [HTTPS/TLS] (instances: %s)", version, *listenAddr, *instanceDir)
		if err := server.ListenAndServeTLS(*tlsCert, *tlsKey); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	} else {
		log.Printf("mpquic-mgmt %s listening on %s [HTTP] (instances: %s)", version, *listenAddr, *instanceDir)
		log.Println("WARNING: running without TLS — use --tls-cert/--tls-key for production")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}
	log.Println("clean exit")
}
