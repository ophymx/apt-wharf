# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo holds

Two related-but-distinct command-line tools share one Go module
(`github.com/ophymx/apt-signpost`):

- **`signpost`** (`cmd/signpost`) — long-running daemon that exposes a signed
  apt repository whose `.deb` requests are HTTP 302-redirected to upstream
  vendors. Hosts only `Release`/`InRelease`/`Packages` metadata; never caches
  `.deb` bytes. Discovers upstream changes via per-source pollers and
  re-publishes when something moves.
- **`cooper`** (`cmd/cooper`) — pure CLI that turns GitHub-released binaries
  into reproducible `.deb` files via two phases joined by a JSON contract
  (`discover` → `plan.json` → `build`). Execs `nfpm pkg` for the actual
  Debian packaging.

A third small helper, `cmd/discover-zoom`, is an example *external producer*
for signpost's discovery contract (Zoom's vendor JSON endpoint). It demonstrates
the `internal/source/external.go` pluggable-producer pattern; it is not part
of either main tool's binary.

The two tools intentionally DON'T overlap: signpost serves apt repos but
doesn't build packages; cooper builds packages but doesn't serve apt repos.
Their natural deployment is side-by-side behind one reverse proxy, with
signpost handling the redirect-style packages and the cooper output going
through a separate apt-repo manager (aptly, reprepro, etc.). The cooper
design doc explicitly leaves "publish .debs to a hosted repo" out of scope.

## Authoritative design docs

The design docs are the source of truth — read them before changing
behavior in either tool.

- **`design.md`** — apt-signpost. Trust model, refresh cycle, source kinds.
- **`cooper-design.md`** — apt-cooper. JSON contract, version selection,
  aux-file resolution, security/sandboxing, reproducibility guarantee.
- **`design-mvp.md`** — earlier signpost MVP iteration; useful context
  but `design.md` overrides where they disagree.

When you change observable behavior, update the relevant design doc in the
same change. When the implementation diverges from the doc, treat that as
a bug to resolve (either by changing the code or updating the doc).

## Common commands

```sh
# Build either binary (the gitignore exempts these names at the repo root):
go build -o signpost ./cmd/signpost
go build -o cooper   ./cmd/cooper

# Local snapshot of all release artifacts (.debs + tarballs + checksums into dist/):
goreleaser release --snapshot --clean

# Default test run — skips integration-tagged and nfpm-binary-required tests:
go test ./...

# Live GitHub integration tests (requires network, optional GITHUB_TOKEN env):
go test -tags integration ./internal/source/...

# Cooper's reproducibility self-check runs only when nfpm is on PATH;
# it auto-skips otherwise.
go test ./internal/cooper/build/ -run TestRun_RealNfpm

# Lint cooper YAML examples without network:
go run ./cmd/cooper validate ./examples/hugo/cooper.yaml

# End-to-end cooper pipeline (needs GITHUB_TOKEN for headroom):
GITHUB_TOKEN=ghp_xxx go run ./cmd/cooper discover ./examples/hugo/cooper.yaml \
  | go run ./cmd/cooper build - --out-dir /tmp/out
```

There is no Makefile, no scripts/, and no CI YAML in the repo.

## Architectural conventions

### Cooper packages are under `internal/cooper/...`

Cooper shares a Go module with signpost. To avoid colliding with
signpost's existing `internal/config`, `internal/source`, etc., every
cooper-internal package lives under `internal/cooper/<name>`. The cooper
design doc's "Package layout" section reflects this. The eventual split
to a standalone `github.com/ophymx/apt-cooper` module is a deferred
import-path rename and shouldn't gate v0 work.

### `pkg/plan` is the public, stable JSON contract

`pkg/plan/` is the only externally-importable cooper package. It defines
the `Plan`/`Package`/`Artifact`/`BuildPlan` types, JCS canonicalization
(RFC 8785), and `ComputeBuildInputsHash`. External producers in any
language can emit a valid `Plan` JSON document and pipe it into
`cooper build`.

Two version knobs control compatibility:
- `plan.SchemaVersion` (currently 1) bumps on incompatible JSON shape changes.
- `plan.FormatRevision` (currently 1) bumps when cooper's output `.deb`
  bytes would diverge for the same logical inputs (e.g. an nfpm major
  upgrade). Independent of cooper release version.

The pinned golden vector in `pkg/plan/hash_test.go`
(`TestComputeBuildInputsHash_Golden`) is the conformance seed for
cross-language producers. Do not edit it without bumping `FormatRevision`.

### Cooper's two-phase split with a JSON boundary

`cooper discover` is read-only and side-effect-free; `cooper build` is
where bytes move. The JSON between them is *also* the plugin point: any
program emitting a valid `Plan` document is a producer that can pipe
into `cooper build`. The orchestrator's dedup decision lives outside
both tools — a third party calls `discover`, filters by querying the
target apt repo for already-imported `build_inputs_hash` values, and
pipes the surviving artifacts into `build`.

`cooper validate` is a third subcommand that runs the full lint
pipeline (config parse, doc-1 schema, aux-file resolution, template
parse + render against placeholder Vars) without touching the network.

### Cooper's reproducibility guarantee

Two `cooper build` invocations against the same JSON plan produce
byte-identical `.debs`. This is enforced via:

- `SOURCE_DATE_EPOCH` from `build_plan.source_date_epoch` (release
  `published_at` → Unix seconds).
- mtime/atime pinning on every staged file before exec'ing nfpm.
- Scrubbed env passed to nfpm: only `VERSION`, `ARCH`, `ASSETS`,
  `SOURCE_DATE_EPOCH`, `LC_ALL=C`, and a fixed minimal `PATH` survive.
- File ownership 0/0 on every entry (cooper rejects non-root
  `owner`/`group` in doc 2's `contents:` entries).
- `expand: true` is auto-set on every contents entry as cooper writes
  `nfpm.yaml`, so nfpm's per-entry env-var expansion fires for
  `${ASSETS}`. This adjustment happens at on-disk-YAML-write time, not
  in `build_plan.nfpm` JSON, so it doesn't perturb `build_inputs_hash`.

`internal/cooper/build/run_e2e_test.go::TestRun_RealNfpm` is the
self-check: it builds the same plan twice and asserts SHA256 equality.
Auto-skips when nfpm isn't installed.

### Signpost's "redirect, don't host" model

Signpost's `Packages` carries the upstream URL plus SHA256/size. apt
clients follow `.deb` redirects and verify the hash, so silent upstream
mutation fails on the client. The refresher's job is to detect upstream
changes and rehash before that happens. Signpost itself does NOT cache
`.deb` bytes — a CDN/caching proxy in front handles that role in
deployment.

### Signpost's discovery contract is pluggable

`internal/source/` defines a `Discoverer` interface with built-in
implementations for `github_release`, `latest_url`, `json_url`, plus an
`external` source kind that exec's a separate program speaking the same
JSON contract on stdin/stdout. `cmd/discover-zoom` is the canonical
example external producer.

## Test patterns and gotchas

- **`//go:build integration`** gates every live-network test. Default
  `go test` skips them; CI/manual runs use `-tags integration`.
- **`internal/cooper/build/run_e2e_test.go`** auto-skips when nfpm isn't
  on PATH. Don't add `t.Fatal` paths that depend on nfpm.
- **`cmd/cooper/examples_test.go::TestExamplesValidateClean`** sweeps
  every `examples/*/cooper.yaml` through `cooper validate`. Adding a new
  example automatically extends the test; that's the regression net.
- **`internal/cooper/source/github_test.go`** uses `httptest` against a
  go-github client whose `BaseURL` is rewritten to the test server.
  Mirror that pattern when adding GitHub API tests.
- **`internal/refresh/bootstrap_stability_test.go`** asserts the
  bootstrap `.deb` is byte-stable across runs — like cooper's
  reproducibility test, but for signpost's self-built keyring package.
- **`pkg/plan/hash_test.go`** is the cross-language-producer corpus
  seed. Touch only when bumping `FormatRevision`.

## Repository layout signals

- `tmp/` is gitignored scratch space — used during recent work to clone
  an old hand-rolled `pkg-builds` codebase for reference. Don't commit
  contents of `tmp/`.
- `dist/` is gitignored; produced by `goreleaser release --snapshot --clean`.
- Both binary names (`/signpost`, `/cooper`) are gitignored at the repo
  root so `go build -o <name>` doesn't pollute the index.
- `.goreleaser.yaml` at the repo root drives binary builds, tarballs, and
  `.deb` packaging for both tools across linux/amd64, arm64, armhf, and
  riscv64. GitHub release publishing is disabled — the config is
  local-build-only. The nfpm-based `.deb` production here is unrelated to
  cooper's own runtime use of nfpm as a build step.
- `examples/` is cooper-only — five copy-paste-ready package
  configurations referenced by the design and validated by the
  regression test. See `examples/README.md` for the per-example
  walkthrough.
- `external/` exposes the small public types signpost's external
  discovery contract uses; importable by third-party producers.
- `packaging/` holds systemd units and shell scripts shipped with
  the signpost `.deb`.
