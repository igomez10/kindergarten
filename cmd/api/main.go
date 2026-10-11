// Command api serves the platform REST API.
//
// Environment:
//
//	DATABASE_URL        metadata database (required)
//	LISTEN_ADDR         default 127.0.0.1:8080 — loopback, behind oauth2-proxy
//	AUTH_MODE           "proxy" (default, trusts oauth2-proxy headers) or "dev" (trusts X-Dev-User)
//	DEV_USER            dev mode: identity for requests without X-Dev-User (browser testing)
//	OIDC_ISSUER         issuer recorded for proxy-mode users (required in proxy mode)
//	NAMESPACE_OVERRIDE  put every Cluster in this namespace (until tenant namespaces exist)
//	MIGRATE_ON_START    "true" to apply the schema at startup (local development)
//	HEALTH_ADDR         unauthenticated /healthz and /readyz only, for kubelet probes; default :8081
//	WEB_DIR             serve the dashboard's static export from this directory
//	KUBERNETES_SECRETS  "true" (default) serves connection details read from instance Secrets
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/igomez10/kindergarten/db"
	"github.com/igomez10/kindergarten/internal/api"
	"github.com/igomez10/kindergarten/internal/auth"
	"github.com/igomez10/kindergarten/internal/config"
	"github.com/igomez10/kindergarten/internal/kubernetes"
	"github.com/igomez10/kindergarten/internal/repository"
)

func main() {
	logger := config.Logger()
	if err := run(logger); err != nil {
		logger.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn, err := config.Require("DATABASE_URL")
	if err != nil {
		return err
	}
	addr := config.Env("LISTEN_ADDR", "127.0.0.1:8080")

	var authn auth.Authenticator
	switch mode := config.Env("AUTH_MODE", "proxy"); mode {
	case "proxy":
		issuer, err := config.Require("OIDC_ISSUER")
		if err != nil {
			return err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return errors.New("AUTH_MODE=proxy trusts identity headers, so LISTEN_ADDR must be a loopback address")
		}
		authn = auth.Proxy(issuer)
	case "dev":
		logger.Warn("AUTH_MODE=dev: any caller can claim any identity with X-Dev-User; never expose this listener")
		authn = auth.DevAs(config.Env("DEV_USER", ""))
	default:
		return errors.New("AUTH_MODE must be proxy or dev, got " + mode)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	if config.Env("MIGRATE_ON_START", "") == "true" {
		if err := db.Migrate(ctx, pool); err != nil {
			return err
		}
	}

	server := api.New(repository.New(pool), authn, api.Config{NamespaceOverride: config.Env("NAMESPACE_OVERRIDE", ""), WebDir: config.Env("WEB_DIR", "")}, logger)
	if config.Env("KUBERNETES_SECRETS", "true") == "true" {
		clients, err := kubernetes.NewClients()
		if err != nil {
			return err
		}
		server.WithSecrets(kubernetes.SecretReader{Kube: clients.Kube})
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Probes come from the kubelet, which cannot reach a loopback listener.
	// This one serves health and nothing else, so exposing it on the pod IP
	// reveals no identity-bearing route.
	health := &http.Server{Addr: config.Env("HEALTH_ADDR", ":8081"), Handler: server.HealthRoutes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health listener", "err", err)
		}
	}()
	defer health.Close()

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
