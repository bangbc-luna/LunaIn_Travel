.PHONY: up run test lint sqlc migrations-sum db-branch db-branch-drop

PG_CONTAINER := lunain-postgres-1
PG_USER := lunain
PG_PORT := 5434
BRANCH_RAW = $(shell git rev-parse --abbrev-ref HEAD)
# Lowercase, every char outside [a-z0-9] -> "_", first 50 chars (DB name <= 63 bytes).
BRANCH_SLUG = $(shell git rev-parse --abbrev-ref HEAD | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]/_/g' | cut -c1-50)
BRANCH_DB = lunain_$(BRANCH_SLUG)

up:
	docker compose up -d

run:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/api

test:
	go test ./...

lint:
	go vet ./...

sqlc:
	go tool sqlc generate

# Rewrites migrations/CHECKSUMS. Only run when ADDING a new migration.
migrations-sum:
	cd internal/platform/db/migrations && sha256sum *.sql | LC_ALL=C sort -k2 > CHECKSUMS

db-branch:
	@set -e; \
	branch='$(BRANCH_RAW)'; db='$(BRANCH_DB)'; \
	if [ -z "$$branch" ] || [ "$$branch" = "HEAD" ]; then echo "detached HEAD: hay checkout mot nhanh truoc khi tao database" >&2; exit 1; fi; \
	found=$$(docker exec $(PG_CONTAINER) psql -U $(PG_USER) -d postgres -tAc "SELECT count(*) FROM pg_database WHERE datname = '$$db'") || { echo "khong truy van duoc Postgres trong container $(PG_CONTAINER) (container dang chay chua?)" >&2; exit 1; }; \
	if [ "$$found" = "1" ]; then \
		echo "database $$db da ton tai, bo qua" >&2; \
	else \
		docker exec $(PG_CONTAINER) createdb -U $(PG_USER) "$$db" || { echo "createdb $$db that bai" >&2; exit 1; }; \
		echo "da tao database $$db" >&2; \
	fi; \
	echo "DATABASE_URL=postgres://$(PG_USER):$(PG_USER)@localhost:$(PG_PORT)/$$db?sslmode=disable"

db-branch-drop:
	@set -e; \
	branch='$(BRANCH_RAW)'; db='$(BRANCH_DB)'; \
	if [ -z "$$branch" ] || [ "$$branch" = "HEAD" ]; then echo "detached HEAD: khong xac dinh duoc nhanh" >&2; exit 1; fi; \
	if [ "$$db" = "lunain" ] || [ "$$db" = "lunain_main" ]; then echo "tu choi xoa database $$db" >&2; exit 1; fi; \
	docker exec $(PG_CONTAINER) dropdb -U $(PG_USER) --if-exists "$$db"; \
	echo "da xoa database $$db"
