# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- Read-through npm registry frontend under `/npm/`: packument caching with
  a freshness TTL, tarball URL rewriting to the mirror, integrity-verified
  tarball downloads (SRI sha512 with legacy sha1 fallback), scoped-package
  support (`@scope/name`, encoded or literal), and empty-document answers
  for the npm CLI's audit/advisory side channels.
- Read-through PyPI simple-index frontend under `/pypi/`: PEP 503 HTML and
  PEP 691 JSON served by content negotiation, PEP 503 name normalization
  with canonical-URL redirects, sha256 URL fragments for pip's hash
  checking, and upstream parsing of both JSON and legacy HTML indexes
  (relative hrefs resolved, `data-requires-python` / `data-yanked` kept).
- Plain-file store (`packument.json` / `index.json` + artifact files with
  sha256sum-compatible `.sha256` sidecars), atomic writes via temp-file
  rename, and strict path-safety validation of names and filenames.
- Flaky-network resilience: immutable artifacts are cached forever, and
  stale metadata is served (marked `X-Depshelf-Source: stale`) when the
  upstream is unreachable; upstream 404s stay authoritative.
- `--offline` mode that never touches the network, and `import` to seed a
  store from local npm tarballs (name/version read from `package.json`)
  and PyPI wheels/sdists (project inferred from the filename), including
  generated packuments with correct integrity fields and dist-tags.
- Deny-by-default `--allowlist` with `npm:` / `pypi:` glob rules, matched
  after PEP 503 normalization.
- `list` (text/JSON store inventory) and `verify` (full re-hash against
  sidecars, exit 1 on corruption) subcommands.
- Runnable examples (`examples/airgap-sync.sh`, `examples/allowlist.example`)
  and a store-format reference (`docs/store-layout.md`).
- 88 deterministic offline tests (unit + in-process HTTP/CLI integration
  against fake loopback upstreams) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/depshelf/releases/tag/v0.1.0
