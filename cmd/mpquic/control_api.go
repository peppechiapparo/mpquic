package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func startControlAPI(ctx context.Context, cfg *Config, mp *multipathConn, logger *Logger) (func(), error) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorizeControlAPI(w, r, cfg) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"role":             cfg.Role,
			"multipath_enabled": cfg.MultipathEnabled,
			"tun_name":         cfg.TunName,
		})
	})

	mux.HandleFunc("/dataplane", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeControlAPI(w, r, cfg) {
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{
				"dataplane": mp.snapshotDataplaneConfig(),
			})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		}
	})

	mux.HandleFunc("/dataplane/validate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorizeControlAPI(w, r, cfg) {
			return
		}

		dp, err := decodeDataplaneFromRequest(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		normalizeDataplaneConfig(&dp, cfg.MultipathPolicy)
		if err := validateDataplaneConfig(dp, cfg.MultipathPaths); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/dataplane/apply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorizeControlAPI(w, r, cfg) {
			return
		}

		dp, err := decodeDataplaneFromRequest(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := mp.applyDataplaneConfig(dp); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("/dataplane/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if !authorizeControlAPI(w, r, cfg) {
			return
		}
		if err := mp.reloadDataplaneFromFile(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	server := &http.Server{
		Addr:    cfg.ControlAPIListen,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go func() {
		logger.Infof("control api listening addr=%s", cfg.ControlAPIListen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf("control api stopped err=%v", err)
		}
	}()

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}, nil
}

func authorizeControlAPI(w http.ResponseWriter, r *http.Request, cfg *Config) bool {
	if cfg.ControlAPIAuthToken == "" {
		return true
	}
	expected := "Bearer " + cfg.ControlAPIAuthToken
	if r.Header.Get("Authorization") != expected {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	return true
}

func decodeDataplaneFromRequest(r *http.Request) (DataplaneConfig, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		return DataplaneConfig{}, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return DataplaneConfig{}, fmt.Errorf("empty request body")
	}

	dp := DataplaneConfig{}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.Contains(ct, "json") {
		if err := json.Unmarshal(body, &dp); err != nil {
			return DataplaneConfig{}, err
		}
		return dp, nil
	}
	if strings.Contains(ct, "yaml") || strings.Contains(ct, "yml") {
		if err := yaml.Unmarshal(body, &dp); err != nil {
			return DataplaneConfig{}, err
		}
		return dp, nil
	}

	if err := json.Unmarshal(body, &dp); err == nil {
		return dp, nil
	}
	if err := yaml.Unmarshal(body, &dp); err != nil {
		return DataplaneConfig{}, fmt.Errorf("payload must be JSON or YAML")
	}
	return dp, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
