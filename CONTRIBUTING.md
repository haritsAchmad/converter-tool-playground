# Contributing

Issues and focused pull requests are welcome.

1. Explain the behavior or threat being addressed.
2. Keep conversion pairs explicitly whitelisted.
3. Never build shell command strings from user input.
4. Add success, malformed-input, traversal, and resource-boundary tests for new formats.
5. Run `go test ./...`, `go vet ./...`, and `go build ./cmd/convertbox`.
6. Update the changelog and documentation when behavior changes.

New external conversion engines also need a restrictive policy, pinned package/container version, non-root execution, timeout, and test corpus.
