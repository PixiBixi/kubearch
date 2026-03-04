.PHONY: build lint test docker snapshot clean

# Build for current platform
build:
	go build -o kubearch .

# Build for all target platforms
build-all:
	GOOS=linux  GOARCH=amd64 go build -o kubearch-linux-amd64 .
	GOOS=linux  GOARCH=arm64 go build -o kubearch-linux-arm64 .
	GOOS=darwin GOARCH=amd64 go build -o kubearch-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 go build -o kubearch-darwin-arm64 .

# Run linters
lint:
	go vet ./...
	staticcheck ./...

# Run tests
test:
	go test -race ./...

# Build local Docker image
docker:
	docker build -t kubearch:local .

# GoReleaser snapshot (dry-run release)
snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -f kubearch kubearch-*
	rm -rf dist/
