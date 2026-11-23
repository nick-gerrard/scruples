SCRUPLES_ADDR ?= 127.0.0.1:8080
SCRUPLES_DB ?= dev.db
SCRUPLES_ADMIN_PASSWORD ?= devpass

.PHONY: dev test vet build

dev:
	SCRUPLES_ADDR=$(SCRUPLES_ADDR) SCRUPLES_DB=$(SCRUPLES_DB) SCRUPLES_ADMIN_PASSWORD=$(SCRUPLES_ADMIN_PASSWORD) go run .

test:
	go test ./...

vet:
	go vet ./...

build:
	go build -o scruples .
