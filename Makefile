.PHONY: build test test-race vet

build:
	go build -o qiniu-exporter ./cmd/qiniu-exporter

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...
