# Contributing to depshelf

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else. The test suite and the smoke script are
fully offline (fake upstreams run in-process on 127.0.0.1).

```bash
git clone https://github.com/JaydenCJ/depshelf && cd depshelf
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary, seeds a store offline via `import`,
serves it, chains a second read-through shelf to the first, kills the
"network" to prove the stale fallback, and checks the allowlist gate and
`verify`; it must finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (88 deterministic tests, no network).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules (protocol parsing and store I/O never share code paths with
   HTTP handling — only `internal/mirror` talks to upstreams).

## Ground rules

- Keep dependencies at zero: depshelf builds from the Go standard library
  only, and that is a headline feature. Adding one needs a very strong PR.
- No network calls beyond the two user-configured upstreams; nothing at
  startup, no telemetry, and `--offline` must remain a hard guarantee.
- The store layout (`docs/store-layout.md`) is a public interface: people
  rsync and grep it. Changing it is a breaking change.
- Every byte served to a client must be integrity-checked on the way into
  the store, or refused; never weaken a digest expectation to "best effort".
- Code comments and doc comments are written in English.
- Determinism first: identical stores must produce byte-identical pages,
  lists and verify reports, including all orderings.

## Reporting bugs

Include the output of `depshelf version`, the full command line, the
relevant request log lines from stderr (they include method, path, status
and cache source), and — for protocol issues — the raw upstream response
(`curl -sD - <upstream-url>`), since that is exactly what the parser saw.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
