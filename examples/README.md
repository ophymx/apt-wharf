# cooper examples

Four end-to-end example packages, each scoped to one cooper feature
surface. Every example validates with:

```sh
cooper validate ./examples/<name>/cooper.yaml
```

(no network required) and is meant to be a copy-paste starting point for
a real package. Numbers below are degrees of complication, not order of
preference — the `gocryptfs` case is what most cooper packages will look
like.

| Example | What it shows |
| --- | --- |
| [`hugo/`](./hugo) | multi-arch tarball; `.tmpl` aux file rendered against `{{ .Version }}` / `{{ .Arch }}` |
| [`talosctl/`](./talosctl) | single-binary asset (no archive); literal aux file (no template) |
| [`gocryptfs/`](./gocryptfs) | flat-layout tarball with multiple binaries + man pages; no aux files |
| [`prometheus/`](./prometheus) | tarball with versioned subdir; literal systemd unit + postinstall script |

## How to run

The full pipeline is `discover → build`, with `validate` as a network-free dry run:

```sh
# 1. Lint without network.
cooper validate ./examples/hugo/cooper.yaml

# 2. Resolve the latest GitHub release into a JSON plan.
GITHUB_TOKEN=ghp_xxx cooper discover ./examples/hugo/cooper.yaml -o /tmp/hugo-plan.json

# 3. Read the plan, fetch + verify assets, exec nfpm, write .deb files.
cooper build /tmp/hugo-plan.json --out-dir /tmp/hugo-out

ls /tmp/hugo-out
# hugo_0.140.0_amd64.deb  hugo_0.140.0_arm64.deb
```

Or as a single shell pipeline:

```sh
GITHUB_TOKEN=ghp_xxx cooper discover ./examples/hugo/cooper.yaml | cooper build - --out-dir /tmp/hugo-out
```

## Per-example notes

### `hugo/` — multi-arch + templated systemd unit

Demonstrates the canonical case from `cooper-design.md`'s
walkthrough: a multi-arch tarball, plus a systemd unit shipped as a
`.tmpl` template that gets rendered against the closed
`{Name, Version, Arch, Epoch, PublishedAt}` variable set.

The doc-2 entry is `src: ./hugo.service`. Because there's no
`hugo.service` on disk but `hugo.service.tmpl` exists, cooper renders
the template at discover time. The rendered bytes land in
`build_plan.aux_files["./hugo.service"]`, and at build time get
materialized at `<staging>/hugo.service` so nfpm finds them at the
path doc 2 wrote.

### `talosctl/` — single-binary asset, no extraction

The upstream release ships an ELF binary directly (no archive).
`IsArchive` returns false, so cooper leaves the file at
`<staging>/asset/talosctl-linux-amd64` and the doc-2 contents entry
references it as `${ASSETS}/talosctl-linux-${ARCH}` — note the
per-arch suffix in the path resolves at discover time, not build
time.

The bash-completion fragment ships as a literal aux file (no
template). It sources the binary's built-in completion at shell
start, which is the simplest deterministic way to support every
`talosctl` subcommand without re-rendering completions in a build job.

### `gocryptfs/` — flat tarball, multiple binaries + man pages

The release tarball unpacks flat (no leading `gocryptfs-1.x.y/`
directory), so every file lands directly under `${ASSETS}/`. Doc 2
just lists each path explicitly. No aux files, no postinstall — pure
nfpm passthrough.

### `prometheus/` — versioned subdir + service + postinstall

The release tarball *does* have a leading directory:
`prometheus-${VERSION}.linux-${ARCH}/`. Cooper does **not** strip it
(per cooper-design.md: "a top-level dir in the archive becomes a
top-level dir under `${ASSETS}/`"). Doc 2 references files inside it
explicitly:

```yaml
- src: ${ASSETS}/prometheus-${VERSION}.linux-${ARCH}/prometheus
  dst: /usr/local/sbin/prometheus
```

`${VERSION}` and `${ARCH}` are substituted at discover time (they
become literals in the plan JSON); `${ASSETS}` is substituted at
build time by nfpm. The systemd unit and postinstall script are
plain literal files in the package directory.

## What you'll need at runtime

- `cooper` itself: `go build -o cooper ./cmd/cooper`.
- `nfpm` on `$PATH` for `cooper build` to work.
  (`cooper validate` and `cooper discover` don't need it.)
- A GitHub token in `GITHUB_TOKEN` for unauthenticated rate-limit
  headroom. Anonymous works for one-off runs but quickly hits the
  60-requests-per-hour anonymous limit.

## What's deliberately *not* covered by these examples

- Non-GitHub sources (zoom, go.dev, JetBrains updates feed). These
  need a `json_url` source kind that is documented in
  `cooper-design.md` §"Deferred (post-v0)".
- Build-time scripts like `make install`, `npm install`, Python venv
  bootstrapping. These belong to a different tool — see the analysis
  in the project root for the carve-out between cooper, signpost,
  and bespoke external producers.
- Apt-repo proxies (hashicorp-release, k8s-release, etc.). Those are
  signpost's job, not cooper's.
