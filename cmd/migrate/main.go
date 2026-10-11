// Command migrate applies the metadata schema and exits. Run as a Job before
// the API and controller roll out.
package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/igomez10/kindergarten/db"
	"github.com/igomez10/kindergarten/internal/config"
)

func main() {
	logger := config.Logger()
	dsn, err := config.Require("DATABASE_URL")
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		logger.Error("connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		logger.Error("migrate", "err", err)
		os.Exit(1)
	}
	logger.Info("schema up to date")
}
