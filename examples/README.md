# depshelf examples

Everything here runs offline against a locally built `depshelf` binary
(`go build -o depshelf ./cmd/depshelf`, then put it on your `PATH` or
prefix the commands with `./`).

## `allowlist.example`

A commented allowlist showing the `npm:` / `pypi:` rule syntax, scope-wide
globs, and PEP 503 normalization of PyPI patterns. Try it:

```bash
depshelf serve --store /tmp/shelf --allowlist examples/allowlist.example
curl -i http://127.0.0.1:8417/npm/not-on-the-list        # -> 403
```

## `airgap-sync.sh`

The full airgap workflow in one script: `import` every npm tarball and
PyPI wheel/sdist from a directory into a fresh store, `verify` it, and
pack it as a tarball for the offline side.

```bash
bash examples/airgap-sync.sh ./my-vendored-artifacts shelf.tar.gz
# transfer shelf.tar.gz however your airgap allows, then on the other side:
tar -xzf shelf.tar.gz
depshelf serve --store ./shelf --offline
```

## Chaining shelves

A depshelf's own endpoints are protocol-compatible upstreams, so a
read-through shelf can front another shelf (e.g. a per-developer cache in
front of a team mirror):

```bash
depshelf serve --store /tmp/team --listen 127.0.0.1:8417 &
depshelf serve --store /tmp/mine --listen 127.0.0.1:8418 \
  --npm-upstream  http://127.0.0.1:8417/npm \
  --pypi-upstream http://127.0.0.1:8417/pypi/simple
```

`scripts/smoke.sh` exercises exactly this topology end to end.
