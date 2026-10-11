TEST_DB_CONTAINER ?= kindergarten-test-db
TEST_DB_PORT      ?= 55432
export KINDERGARTEN_TEST_DATABASE_URL ?= postgres://postgres:postgres@127.0.0.1:$(TEST_DB_PORT)/postgres?sslmode=disable

.PHONY: build test test-db test-db-stop fmt vet

build:
	go build ./...

# Unit and integration tests. Integration tests need `make test-db`.
test:
	go test -race -count=1 ./...

# A disposable Postgres for the integration tests.
test-db:
	@docker inspect $(TEST_DB_CONTAINER) >/dev/null 2>&1 || \
		docker run -d --name $(TEST_DB_CONTAINER) -e POSTGRES_PASSWORD=postgres -p 127.0.0.1:$(TEST_DB_PORT):5432 postgres:17
	@until docker exec $(TEST_DB_CONTAINER) pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done

test-db-stop:
	docker rm -f $(TEST_DB_CONTAINER)

fmt:
	gofmt -w cmd internal db

vet:
	go vet ./...
