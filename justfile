default: test

# Every Go build needs FTS5 compiled into sqlite (for message search).
export GOFLAGS := "-tags=sqlite_fts5"

# Install the pre-commit hooks (gofmt, go vet, gitleaks).
setup:
    go mod download
    pre-commit install

test:
    go test ./...

# Format and vet (same checks as the pre-commit hooks).
check:
    gofmt -w .
    go vet ./...
