# dd-micro-agent

BIN := bin/agent

.PHONY: build test race vet fmt clean

# Fully static, CGO-free binary.
build:
	CGO_ENABLED=0 go build -tags netgo -ldflags "-s -w" -o $(BIN) ./cmd/agent

test:
	go test ./...

# The race detector needs cgo, which the static-build workflow leaves disabled.
race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

# Format code and organize imports. goimports is a superset of gofmt, and
# -local groups this module's own packages into their own import block.
# Install once: go install golang.org/x/tools/cmd/goimports@latest
fmt:
	goimports -l -w -local github.com/0intro/dd-micro-agent .

clean:
	rm -rf bin
