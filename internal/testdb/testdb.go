// Package testdb gives each test its own freshly migrated metadata database.
//
// Point PGCLOUD_TEST_DATABASE_URL at a disposable Postgres server (`make
// test-db` starts one in Docker). Tests that need it are skipped without it.
// Never point it at a real metadata database: each test creates and drops a
// database named pgcloud_test_<random> on that server.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/igomez10/kindergarten/db"
)

func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("PGCLOUD_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("PGCLOUD_TEST_DATABASE_URL not set; run `make test-db` and export it")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	name := "pgcloud_test_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
		adminPool.Close()
	})
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
