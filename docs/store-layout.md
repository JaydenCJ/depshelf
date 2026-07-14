# The depshelf store layout

The store is the whole product: plain files in a transparent tree that you
can rsync, grep, back up with `cp -r`, and audit with `sha256sum -c`. This
document is the reference for that layout. It is a public interface —
changing it is a breaking change.

## Tree

```
<store>/
├── .tmp/                                  # atomic-write staging (always empty at rest)
├── npm/
│   ├── left-pad/
│   │   ├── packument.json                 # upstream-form packument, verbatim
│   │   └── tarballs/
│   │       ├── left-pad-1.3.0.tgz
│   │       └── left-pad-1.3.0.tgz.sha256  # "<hex>  <filename>" (sha256sum -c compatible)
│   └── @babel/
│       └── core/                          # scoped packages nest one level deeper
│           ├── packument.json
│           └── tarballs/…
└── pypi/
    └── typing-extensions/                 # always the PEP 503 normalized name
        ├── index.json                     # canonical PEP 691 JSON, files sorted by filename
        └── files/
            ├── typing_extensions-4.12.0-py3-none-any.whl
            └── typing_extensions-4.12.0-py3-none-any.whl.sha256
```

## Rules

- **Metadata is stored upstream-form.** `packument.json` keeps the original
  tarball URLs; rewriting to the mirror's own URLs happens at serve time,
  so the same store works no matter which host or port serves it. PyPI
  indexes are converted to canonical PEP 691 JSON on the way in (even when
  the upstream spoke legacy PEP 503 HTML) with files sorted by filename, so
  identical content is byte-identical on disk.
- **Artifacts are immutable.** Once a tarball/wheel is on disk it is never
  re-fetched. Every artifact was integrity-checked against the digest its
  metadata advertised (SRI sha512, legacy sha1, or PEP 503 sha256) before
  the file appeared, and gets a `.sha256` sidecar recording what was
  actually stored. `depshelf verify` re-hashes everything against sidecars.
- **Writes are atomic.** Everything lands via a temp file in `<store>/.tmp`
  plus `rename(2)`, so a reader (or a second depshelf process) never sees a
  partial file, and a failed or tampered download leaves no trace.
- **Names are the path-safety gate.** npm names must match the registry
  naming rules, PyPI directories must be PEP 503 normalized, and artifact
  filenames must match `^[A-Za-z0-9][A-Za-z0-9._+-]*$` (with `.sha256`
  reserved for sidecars). Anything else is rejected before touching disk.
- **Freshness is the file's mtime.** Metadata older than `--metadata-ttl`
  (default 15m) is revalidated against the upstream; in `--offline` mode
  any cached copy is served regardless of age. You can force revalidation
  with `touch -t 202001010000 <…>/packument.json`.

## The `imported/<file>` URL convention

`depshelf import` generates metadata for artifacts that never came from an
upstream. Their `tarball`/`url` fields are recorded as `imported/<filename>`
— a marker, not a fetchable URL. It is never dereferenced: the artifact is
already on disk, and serving only needs the basename to route the download
back through the mirror.

## Moving a store to an airgapped machine

The store is self-contained, so transfer is just file copy:

```bash
tar -C /var/cache -czf shelf.tar.gz depshelf-store   # on the connected side
tar -C /var/cache -xzf shelf.tar.gz                  # on the airgapped side
depshelf verify --store /var/cache/depshelf-store    # prove nothing rotted in transit
depshelf serve  --store /var/cache/depshelf-store --offline
```

See `examples/airgap-sync.sh` for a scripted version.
