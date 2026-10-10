package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shy/server/internal/config"
	"shy/server/internal/httpapi"
	"shy/server/internal/neatlogs"
	"shy/server/internal/store"
	"shy/server/internal/version"
)

func main() {
	if len(os.Args) > 1 {
		usage()
		if os.Args[1] == "-h" || os.Args[1] == "--help" {
			os.Exit(0)
		}
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("database", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	var fwd neatlogs.Forwarder = neatlogs.Disabled{}
	mode := "store-only"
	if cfg.NeatlogsAPIKey != "" {
		client, err := neatlogs.Open(context.Background(), cfg.NeatlogsAPIKey, cfg.NeatlogsIngestURL, cfg.NeatlogsProject)
		if err != nil {
			slog.Error("neatlogs", "err", err)
			_ = st.Close()
			os.Exit(1)
		}
		defer func() {
			shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := client.Close(shut); err != nil {
				slog.Error("neatlogs shutdown", "err", err)
			}
		}()
		fwd = client
		mode = "forwarding"
	} else {
		slog.Warn("SHY_NEATLOGS_API_KEY is unset; traces are stored and not forwarded")
	}

	app, err := httpapi.New(httpapi.Options{Config: cfg, Store: st, Forward: fwd})
	if err != nil {
		slog.Error("http", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()

	slog.Info("listening", "addr", cfg.Addr, "db", cfg.DBPath, "mode", mode, "version", version.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `shyd %s — collect shy traces and forward them with the Neatlogs Go SDK.

Listens on SHY_HTTP_ADDR (default :8080). The SQLite file is SHY_DB_PATH (default shy.db).

Point shy at this server. An install key attributes the trace to an account.
A request with no key is accepted as anonymous.

  export NEATLOGS_INGEST_URL=http://127.0.0.1:8080
  export NEATLOGS_API_KEY=shy_...

Environment:
  SHY_HTTP_ADDR              listen address (default :8080)
  SHY_DB_PATH                SQLite file (default shy.db)
  SHY_PUBLIC_URL             base URL shown in the dashboard
  SHY_NEATLOGS_API_KEY       project key sent as x-api-key by the Go SDK
  SHY_NEATLOGS_PROJECT_NAME  required for nlw_ write keys; sent as a resource tag
  SHY_NEATLOGS_INGEST_URL    OTLP base URL (default https://ingest.neatlogs.com)
  SHY_BODY_LIMIT             max trace body in bytes (default 8388608, max 52428800)
`, version.Version)
}
