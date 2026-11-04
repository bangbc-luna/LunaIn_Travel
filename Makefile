.PHONY: up run test lint sqlc

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
