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
	listenAddr := flag.String("listen", ":8080", "HTTP listen address")
	instanceDir := flag.String("instance-dir", "/etc/mpquic/instances", "directory containing tunnel YAML configs")
	authToken := flag.String("auth-token", "", "Bearer token for API auth (or MGMT_AUTH_TOKEN env)")
	flag.Parse()

	// Auth token from flag or env
	token := *authToken
	if token == "" {
		token = os.Getenv("MGMT_AUTH_TOKEN")
	}

	mgr := NewInstanceManager(*instanceDir)
	h := NewAPIHandler(mgr, token)

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

	log.Printf("mpquic-mgmt %s listening on %s (instances: %s)", version, *listenAddr, *instanceDir)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	log.Println("clean exit")
}
