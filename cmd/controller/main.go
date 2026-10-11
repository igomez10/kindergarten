// Command controller reconciles instance rows into CloudNativePG Clusters.
//
// Environment:
//
//	DATABASE_URL    metadata database (required)
//	STORAGE_CLASS   for database volumes, default longhorn
//	CONN_CHECK      "direct" (in-cluster, default) or "portforward" (running on a laptop)
//	WORKERS         concurrent reconciliations, default 4
//	CONTROLLER_ID   lease owner name, default hostname
//	METRICS_ADDR    health endpoint address, default :8081
//	DB_DOMAIN       expose instances as <name>.<DB_DOMAIN> on LoadBalancer IPs; empty = cluster-internal
//	ADDRESS_POOL    MetalLB pool for those IPs, default pgcloud-db
//	CLOUDFLARE_API_TOKEN, CLOUDFLARE_ZONE   publish <name>.<DB_DOMAIN> A records; both or neither
//	TENANT_BOOTSTRAP  "true" (default) creates each organization's namespace, quota and policies
//	PLATFORM_NAMESPACE  default platform-system; OPERATOR_NAMESPACE default cnpg-system
//	TENANT_SECRET_READERS  comma-separated ServiceAccounts in PLATFORM_NAMESPACE bound to
//	                       TENANT_SECRET_READER_ROLE in every tenant namespace
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/igomez10/kindergarten/internal/config"
	"github.com/igomez10/kindergarten/internal/controller"
	"github.com/igomez10/kindergarten/internal/dns"
	"github.com/igomez10/kindergarten/internal/kubernetes"
	"github.com/igomez10/kindergarten/internal/provisioner/postgres"
	"github.com/igomez10/kindergarten/internal/repository"
	"github.com/igomez10/kindergarten/internal/tenant"
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
	workers, err := strconv.Atoi(config.Env("WORKERS", "4"))
	if err != nil {
		return fmt.Errorf("WORKERS: %w", err)
	}
	host, _ := os.Hostname()
	owner := config.Env("CONTROLLER_ID", host) + "-" + uuid.NewString()[:6]

	clients, err := kubernetes.NewClients()
	if err != nil {
		return err
	}
	var checker postgres.ConnChecker
	switch mode := config.Env("CONN_CHECK", "direct"); mode {
	case "direct":
		checker = kubernetes.DirectChecker{}
	case "portforward":
		checker = kubernetes.PortForwardChecker{Clients: clients}
	default:
		return fmt.Errorf("CONN_CHECK must be direct or portforward, got %q", mode)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	var dnsProvider dns.Provider
	token, zone := os.Getenv("CLOUDFLARE_API_TOKEN"), os.Getenv("CLOUDFLARE_ZONE")
	switch {
	case token != "" && zone != "":
		dnsProvider = dns.NewCloudflare(token, zone)
	case token != "" || zone != "":
		return fmt.Errorf("set both CLOUDFLARE_API_TOKEN and CLOUDFLARE_ZONE, or neither")
	}
	opts := postgres.Options{
		StorageClass: config.Env("STORAGE_CLASS", "longhorn"),
		DBDomain:     os.Getenv("DB_DOMAIN"),
		AddressPool:  config.Env("ADDRESS_POOL", "pgcloud-db"),
	}
	prov := postgres.New(clients.Dynamic, clients.Kube, checker, dnsProvider, opts)
	ctrl := controller.New(repository.New(pool), prov, controller.Config{Owner: owner, Workers: workers}, logger)
	if config.Env("TENANT_BOOTSTRAP", "true") == "true" {
		ctrl.WithTenants(tenant.New(clients.Dynamic, tenant.Options{
			PlatformNamespace: config.Env("PLATFORM_NAMESPACE", "platform-system"),
			OperatorNamespace: config.Env("OPERATOR_NAMESPACE", "cnpg-system"),
			SecretReaders:     splitList(os.Getenv("TENANT_SECRET_READERS")),
			SecretReaderRole:  config.Env("TENANT_SECRET_READER_ROLE", "pgcloud-tenant-secrets-reader"),
		}))
	}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			if err := pool.Ping(r.Context()); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		if err := http.ListenAndServe(config.Env("METRICS_ADDR", ":8081"), mux); err != nil {
			logger.Error("health server", "err", err)
		}
	}()

	go ctrl.Listen(ctx, pool)
	logger.Info("controller started", "owner", owner, "workers", workers, "db_domain", opts.DBDomain, "dns", dnsProvider != nil)
	ctrl.Run(ctx)
	logger.Info("controller stopped")
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
