# apt-wharf

A family of five Go CLIs for building, signing, and serving Debian apt
repositories without re-hosting vendor binaries.

The vendor install ritual —

```sh
curl https://example.com/keys/foo.gpg | sudo tee /etc/apt/trusted.gpg.d/foo.gpg
echo 'deb https://example.com/repo bookworm main' | sudo tee /etc/apt/sources.list.d/foo.list
curl -O https://example.com/foo-1.2.3_amd64.deb && sudo dpkg -i foo-1.2.3_amd64.deb
```

— is fragile, unreproducible, and undocumented. apt-wharf turns each of
those steps into a reproducible `.deb` plus a signed apt repo that points
at the upstream URLs without re-hosting them.

## The five tools

| Binary | Role |
| --- | --- |
| [`signpost`](./cmd/signpost) | Daemon. Serves a signed apt repo whose `.deb` requests HTTP-redirect to upstream vendors. Hosts only `Release` / `InRelease` / `Packages` metadata; never caches `.deb` bytes. |
| [`cooper`](./cmd/cooper) | CLI. Turns GitHub-released binaries into reproducible `.deb` files via a two-phase JSON contract (`discover` → `plan.json` → `build`). Execs `nfpm pkg` for the actual Debian packaging. |
| [`chandler`](./cmd/chandler) | CLI. Turns the `curl URL \| sudo tee ...` keyring + sources install ritual into a reproducible keyring + `.sources` `.deb`. Fetches keys over HTTPS, dearmors in-process, renders deb822. |
| [`staves`](./cmd/staves) | CLI. Packs locally-checked-in files (configs, systemd units, scripts) into a `.deb` via cooper's build pipeline. `source_date_epoch` derives from `git log` of the package directory. |
| [`drayman`](./cmd/drayman) | CLI. Consumes cooper / chandler / staves output, dedups against the target apt repo by `X-Cooper-Build-Inputs-Hash`, applies revision auto-bump policy, and pushes `.debs` into aptly or reprepro. |

The names are a cooperage metaphor: a *cooper* assembles barrels from
*staves* (planks), a *chandler* supplies ships with provisions, a
*drayman* hauls cargo to the dock, and a *signpost* tells everyone
which berth to find. The umbrella project is the *wharf*.

The tools intentionally don't overlap by scope. cooper handles upstream
binary releases, staves handles in-house files, chandler handles vendor
keyrings, drayman ferries the resulting `.deb`s into a repo, and signpost
fronts vendor URLs directly (no `.deb` build needed).

## How they fit together

```
                upstream GitHub release
                          │
                          ▼
                   ┌─────────────┐
local files ─────► │   cooper /  │ ───► plan.json ───► cooper build ───► .deb
   via staves      │   chandler  │
vendor keys ─────► │   / staves  │
   via chandler    └─────────────┘
                                                          │
                                                          ▼
                                                      drayman ───► aptly / reprepro
                                                                         │
                                                                         ▼
                                                                   signed apt repo

                       (or, no build at all:)

upstream vendor URL ─────────────────────────────► signpost ───► signed apt repo
                                                  (302 redirect to vendor URL,
                                                   client verifies hash)
```

## Status

Pre-v0.1.0. No tagged release yet; install from source.

The design docs are the source of truth for each tool's behavior:

- [`design.md`](./design.md) — signpost. Trust model, refresh cycle, source kinds.
- [`cooper-design.md`](./cooper-design.md) — cooper. JSON contract, version
  selection, aux-file resolution, reproducibility guarantee.
- [`chandler-design.md`](./chandler-design.md) — chandler. Schema, templating
  (matrix mode), trust model (HTTPS-only), deb822 rendering, conffile policy.

[`CLAUDE.md`](./CLAUDE.md) is the per-repo orientation doc for working in
the tree.

## Build

```sh
go build -o signpost ./cmd/signpost
go build -o cooper   ./cmd/cooper
go build -o chandler ./cmd/chandler
go build -o staves   ./cmd/staves
go build -o drayman  ./cmd/drayman
```

Each binary embeds its commit and build date (via `runtime/debug` for
`go build`, via goreleaser ldflags for tagged releases):

```sh
$ ./cooper --version
cooper dev (commit 3bd00549b1a2, built 2026-05-29T01:25:34Z)
```

Local snapshot of `.deb`s + tarballs for all five binaries across
linux/amd64, arm64, armhf, riscv64:

```sh
goreleaser release --snapshot --clean
ls dist/
```

`cooper build` requires `nfpm` on `$PATH`. The other phases
(`cooper validate`, `cooper discover`, `chandler validate`,
`chandler discover`, `staves validate`, `staves discover`) don't.

## Quick start

The canonical pipeline is `discover` → `build`, where `discover` is
network-bound and side-effect-free, `build` writes `.deb` bytes, and the
JSON between them is also the public contract third-party producers
speak.

Package the latest [Hugo](https://gohugo.io) release:

```sh
# 1. Lint without network.
cooper validate ./examples/hugo/cooper.yaml

# 2. Resolve GitHub → JSON plan.
GITHUB_TOKEN=ghp_xxx cooper discover ./examples/hugo/cooper.yaml -o /tmp/hugo.json

# 3. Fetch + verify assets, exec nfpm, write .deb files.
cooper build /tmp/hugo.json --out-dir /tmp/out
ls /tmp/out
# hugo_0.140.0_amd64.deb  hugo_0.140.0_arm64.deb
```

Or as a single pipeline:

```sh
GITHUB_TOKEN=ghp_xxx cooper discover ./examples/hugo/cooper.yaml \
  | cooper build - --out-dir /tmp/out
```

Replace `cooper discover ...` with `chandler discover ./examples/hashicorp/chandler.yaml`
or `staves discover ./examples/staves-demo/staves.yaml` to feed cooper from a
different producer. See [`examples/README.md`](./examples/README.md) for the
per-example walkthrough.

## Reproducibility

Two `cooper build` invocations against the same JSON plan produce
byte-identical `.deb`s. The guarantee covers `SOURCE_DATE_EPOCH` pinning
(from the release's `published_at`), mtime/atime pinning on every staged
file, a scrubbed env passed to nfpm, and root-owned tar entries. The
self-check lives at `internal/cooper/build/run_e2e_test.go::TestRun_RealNfpm`
and runs only when nfpm is on `$PATH`.

The `Plan` JSON contract (`pkg/plan/`) carries a stable
`build_inputs_hash` derived via JCS canonicalization (RFC 8785). It's
the conformance seed for cross-language producers and the dedup key
drayman uses against the target repo.

## Running in CI

`staves` and `chandler` derive `source_date_epoch` from
`git log -- <package-or-config-path>`. **Your CI checkout must
include enough history that this returns the actual last-touch
commit, not just HEAD.** In a shallow clone (the default for
`actions/checkout@v4` with no `fetch-depth` set is depth=1) git
log returns HEAD's timestamp regardless of whether HEAD actually
touched the file, which:

- Changes `source_date_epoch` on every push (workflow trigger
  time becomes the commit time the recipe sees).
- Destabilizes `build_inputs_hash` for every staves/chandler
  package across runs.
- Forces drayman's auto-bump policy to ratchet the debian
  revision on every push, eventually producing artifact names
  like `your-package_1.2.3-27_amd64.deb` for a package that
  never changed.

Both `staves discover` and `chandler discover` hard-error when
they detect a shallow clone. The fix is to fetch the real
history:

```yaml
# actions/checkout@v4
- uses: actions/checkout@v4
  with:
    fetch-depth: 0
```

Or in plain git:

```sh
git fetch --unshallow
```

If you're certain HEAD is the relevant commit (a one-off local
build at a pinned ref, for example), pass `--allow-shallow` to
bypass the gate. Don't use this in CI — it papers over the
problem rather than fixing it.

## Repo layout

- [`cmd/`](./cmd) — one main package per shipped binary.
- [`internal/`](./internal) — per-tool packages organized as
  `internal/<tool>/<name>/` for the four newer CLIs; signpost's older
  packages sit at `internal/{config,source,sign,refresh,…}/`.
- [`pkg/plan/`](./pkg/plan) — the only externally-importable cooper
  package: the `Plan` / `Package` / `Artifact` / `BuildPlan` types, JCS
  canonicalization, and `ComputeBuildInputsHash`.
- [`external/`](./external) — small public types for signpost's external
  discovery contract.
- [`examples/`](./examples) — copy-paste-ready configs for cooper,
  chandler, and staves, plus
  [`external-producers/`](./examples/external-producers) holding
  buildable reference programs that implement signpost's stdio
  external-discoverer contract. See
  [`examples/README.md`](./examples/README.md).
- [`packaging/`](./packaging) — systemd units and maintainer scripts
  shipped in the signpost `.deb`.
- [`scripts/precommit-checks.sh`](./scripts/precommit-checks.sh) — runs
  `go fmt`, `go vet`, `go fix`, `go build`, and `go test ./...`.
- [`.goreleaser.yaml`](./.goreleaser.yaml) — local-build pipeline for all
  five binaries; GitHub release publishing is disabled.

## Testing

```sh
go test ./...                                  # default: skips integration + nfpm-required
go test -tags integration ./internal/source/...  # live GitHub
```

Cooper's and chandler's reproducibility self-checks run only when `nfpm`
is on `$PATH`; they auto-skip otherwise.

## License

[MIT](./LICENSE). All runtime dependencies are MIT, Apache-2.0, BSD-2/3,
or public domain — none copyleft.
