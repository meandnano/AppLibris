.PHONY: build run test

build:
	go build -o bin/server ./cmd/server

run:
	LIBRARY_DIR=./library COVERS_DIR=./data/covers DB_PATH=./data/library.db go run ./cmd/server

test:
	go test ./...
