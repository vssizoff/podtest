package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"podtest/internal/artifact"
	"podtest/internal/auth"
	"podtest/internal/authorization"
	"podtest/internal/db"
	"podtest/internal/judging"
	"podtest/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("backend stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mode := "api"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "fga-load" {
		return authorization.Load(ctx, os.Getenv("FGA_URL"), os.Getenv("FGA_TOKEN"), os.Getenv("FGA_STORE_ID"))
	}
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	if mode == "migrate" {
		return db.Migrate(ctx, pool)
	}
	fga := &authorization.HTTPClient{URL: os.Getenv("FGA_URL"), Token: os.Getenv("FGA_TOKEN"), Store: os.Getenv("FGA_STORE_ID"), Model: os.Getenv("FGA_MODEL_ID"), StateFile: os.Getenv("FGA_STATE_FILE")}
	if mode == "worker" {
		workerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() { _ = authorization.RunOutbox(workerCtx, pool, fga) }()
		return judging.RunWorker(workerCtx, pool, os.Getenv("NATS_URL"))
	}
	store, err := artifact.New(pool)
	if err != nil {
		return err
	}
	if mode == "storage-init" {
		return store.Init(ctx)
	}
	if mode == "invoker-artifacts" {
		return serveInvokerArtifacts(ctx, pool, store)
	}
	publicURL := strings.TrimRight(os.Getenv("PUBLIC_URL"), "/")
	provider, err := auth.NewProvider(ctx, os.Getenv("OIDC_ISSUER_URL"), os.Getenv("OIDC_CLIENT_ID"), os.Getenv("OIDC_CLIENT_SECRET"), publicURL+"/auth/callback")
	if err != nil {
		return err
	}
	sessions, err := auth.NewSessions(pool, provider, publicURL, os.Getenv("APP_ENV") == "development")
	if err != nil {
		return err
	}
	server := &http.Server{Addr: ":8080", Handler: web.Router(pool, sessions, authorization.NewManager(fga), store), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	slog.Info("backend listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
