# Examples

Each `examples/<vendor>/` directory holds the YAML for whichever tool(s)
apply to that vendor; filenames distinguish them — `cooper.yaml` for
cooper recipes, `chandler.yaml` for chandler recipes. Both tools'
regression tests glob their respective filename pattern across this
directory, so the two example sets coexist without colliding.

## cooper examples

Eight end-to-end example packages, each scoped to one cooper feature
surface. Every example validates with:

```sh
cooper validate ./examples/<name>/cooper.yaml
```

(no network required) and is meant to be a copy-paste starting point for
a real package. Order in the table below is roughly increasing
complication; the `gocryptfs` case is what most cooper packages will look
like.

| Example | What it shows |
| --- | --- |
| [`hugo/`](./hugo) | multi-arch tarball; `.tmpl` aux file rendered against `{{ .Version }}` / `{{ .Arch }}` |
| [`talosctl/`](./talosctl) | single-binary asset (no archive); literal aux file (no template) |
| [`gocryptfs/`](./gocryptfs) | flat-layout tarball with multiple binaries + man pages; no aux files |
| [`prometheus/`](./prometheus) | tarball with versioned subdir; literal systemd unit + postinstall script |
| [`ollama/`](./ollama) | one cooper.yaml producing **two** `.deb`s from the same upstream asset; nfpm `type: tree` for shipping subdirectories |
| [`jetbrains-toolbox/`](./jetbrains-toolbox) | `json_url` source kind; gjson placeholders in `asset_url` (per-arch download links from the same JSON body) |
| [`intellij-idea-community/`](./intellij-idea-community) | `xml_url` source kind; XPath against JetBrains' `updates.xml` feed |
| [`cfssl/`](./cfssl) | **multi-asset per arch** — eight independent binaries from one release staged side-by-side under `${ASSETS}/` |

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

### `ollama/` — one cooper.yaml, two `.deb`s, shared asset

The most interesting layout. `cooper.yaml` lists two per-package
files; both pull from the **same** GitHub release
(`ollama/ollama` → `ollama-linux-${ARCH}.tgz`) and slice different
paths out of the extracted tree:

- `ollama` ships `bin/ollama` plus a literal systemd unit and a
  postinstall script that creates the `ollama` system user.
- `libollama-nvidia` ships the CUDA-flavored ggml runners under
  `lib/ollama/cuda_v12/` and `lib/ollama/cuda_v13/` using nfpm's
  `type: tree`, which walks a source directory recursively at build
  time and copies every regular file under it. The two-tree pattern
  means a future tarball that adds `cuda_v14/` is a one-line change.

Cooper extracts the whole tarball regardless of which subset each
package needs; the actual partitioning happens via doc 2's
`contents:` list. nfpm only sees the files it's told to ship, and
that's what ends up in each `.deb`. The two builds are independent
artifacts with distinct `build_inputs_hash` values, so the
orchestrator can dedup them separately against the target apt repo.

`ollama` declares `Recommends: libollama-nvidia` so apt offers to
pull GPU support alongside the binary on NVIDIA hosts but doesn't
require it on CPU-only ones.

### `jetbrains-toolbox/` — `json_url` source kind

JetBrains publishes per-product release metadata as JSON at
`data.services.jetbrains.com/products/releases`. Cooper fetches the
endpoint once, extracts the build number via `gjson` (`TBA.0.build`),
and renders each arch's download URL by interpolating
`{TBA.0.downloads.linux.link}`-style placeholders against the same
response. No GitHub release; no external producer.

### `cfssl/` — multi-asset per arch

Cloudflare's cfssl release ships eight independent binaries per
architecture — `cfssl`, `cfssljson`, `cfssl-bundle`, `cfssl-certinfo`,
`cfssl-newkey`, `cfssl-scan`, `mkbundle`, `multirootca` — with no
bundling archive. The recipe uses `arches[].assets:` (plural) to
enumerate every selector; cooper resolves each against the release,
stages them side-by-side under `${ASSETS}/`, and the nfpm `contents:`
list installs all eight to `/usr/bin/`.

The same shape applies to any release that publishes loose binaries
instead of a tarball. The singular `arches[].asset:` form remains
sugar for the single-binary case; recipes only reach for plural when
the release actually contains independent files.

Asset-name collisions (two selectors resolving to the same release
asset) are rejected at discover time — both selectors land under one
`${ASSETS}/` directory, so identical resolved basenames can't both
exist.

### `intellij-idea-community/` — `xml_url` source kind

JetBrains' canonical updates feed at
`www.jetbrains.com/updates/updates.xml` covers every IntelliJ-platform
product and channel. Cooper's `xml_url` source extracts the current
release build number via XPath
(`//product[@name='IntelliJ IDEA']/channel[@status='release']/build/@number`)
and uses standard `${VERSION}`/`${ARCH}` substitutions in the per-arch
`asset_url` templates. The `xml_url` kind also accepts
`{xpath:<expr>}` placeholders for vendors who embed download links in
the same response body (mirroring `json_url`'s `{gjson.path}` slot).

## chandler examples

Five end-to-end keyring + sources packages, each scoped to one
chandler feature surface. Replaces the typical
`curl URL | sudo tee /etc/apt/sources.list.d/...` install ritual with
a reproducible, version-tracked `.deb`. Every example validates with:

```sh
chandler validate ./examples/<name>/chandler.yaml
```

(no network required). Order is roughly increasing complication; the
`docker-ce` case is what a minimal chandler config looks like.

| Example | What it shows |
| --- | --- |
| [`docker-ce/`](./docker-ce) | simple mode (no matrix), single source, hardcoded `bookworm` suite |
| [`hashicorp/`](./hashicorp) | matrix across 4 codenames (debian + ubuntu), single source, `{{ .Codename }}` in `suites:` |
| [`postgresql/`](./postgresql) | matrix + two sources (pgdg + pgdg-testing) sharing one key |
| [`tailscale/`](./tailscale) | `{{ .Distro }}` + `{{ .Codename }}` templated into both the key URL and the repo URI |
| [`nvidia-cuda/`](./nvidia-cuda) | flat ("trivial") repository — `suites: /`, components absent, per-arch URIs and per-arch keys |

Each YAML carries the verbatim upstream `curl|tee` snippet as a
header comment, so the file reads as a side-by-side translation
between the vendor's instructions and chandler's structured form.

### How to run chandler examples

The full pipeline is `chandler discover → cooper build`, with
`chandler validate` as a network-free dry run:

```sh
# 1. Lint without network.
chandler validate ./examples/hashicorp/chandler.yaml

# 2. Fetch keys over HTTPS, render .sources stanzas, emit a plan.
chandler discover ./examples/hashicorp/chandler.yaml -o /tmp/hc-plan.json

# 3. Read the plan, stage aux_files, exec nfpm, write .deb files.
cooper build /tmp/hc-plan.json --out-dir /tmp/hc-out

ls /tmp/hc-out
# hashicorp-archive-keyring-bookworm_1.1_all.deb
# hashicorp-archive-keyring-trixie_1.1_all.deb
# hashicorp-archive-keyring-jammy_1.1_all.deb
# hashicorp-archive-keyring-noble_1.1_all.deb
```

Or as a single shell pipeline:

```sh
chandler discover ./examples/hashicorp/chandler.yaml | cooper build - --out-dir /tmp/hc-out
```

chandler needs no GitHub token — it only fetches the vendor key URL
named in `keys:`. Git is optional: chandler records `git_commit` /
`git_date` on `plan.Source` as best-effort provenance when the config
file lives in a git working tree, but neither field influences
`source_date_epoch` (which is content-derived) or `build_inputs_hash`.

## What you'll need at runtime

- `cooper` itself: `go build -o cooper ./cmd/cooper`.
- `chandler` for chandler examples: `go build -o chandler ./cmd/chandler`.
- `nfpm` on `$PATH` for `cooper build` to work.
  (`cooper validate`, `cooper discover`, `chandler validate`, and
  `chandler discover` don't need it.)
- A GitHub token in `GITHUB_TOKEN` for cooper examples — unauthenticated
  rate-limit headroom. Anonymous works for one-off runs but quickly hits
  the 60-requests-per-hour anonymous limit. chandler doesn't need it
  (vendor key URLs aren't rate-limited).

## What's deliberately *not* covered by these examples

- Build-time scripts like `make install`, `npm install`, Python venv
  bootstrapping. These belong to a different tool — see the analysis
  in the project root for the carve-out between cooper, signpost,
  and bespoke external producers.
- Apt-repo proxies (hashicorp-release, k8s-release, etc.). Those are
  signpost's job, not cooper's.
- HTML-scraped sources — vendors that publish download metadata only
  on a rendered page (developer.android.com, flutter.dev, Apache
  directory-studio). **Out of scope** for cooper itself: a CSS-selector-
  based recipe would carry too much fragility per recipe versus the
  alternative. Use an external producer that scrapes the page and
  emits a `plan.Plan` JSON document; see cooper-design.md §"HTML-
  scraped sources" for the rationale and a ~30-line shell example.

## `external-producers/`

[`external-producers/`](./external-producers) holds buildable
reference programs implementing signpost's stdio external-discoverer
contract — not shipped binaries, just worked examples kept around to
illustrate the full contract.

| Example | What it shows |
| --- | --- |
| [`discover-zoom/`](./external-producers/discover-zoom) | the canonical stdin/stdout contract against Zoom's `result.downloadVO.zoom.version` JSON endpoint |

Reach for the `external` source kind only when the built-in
`json_url` / `xml_url` discoverers genuinely can't represent the
vendor's discovery shape (multi-step auth, cookies, non-JSON/XML
formats). The Zoom case shown here is itself now covered by a
five-line `json_url` config (see signpost's `design.md` §"Built-in:
json_url"); the binary remains as a worked reference for the stdio
protocol, not a recommendation to use this pattern when a declarative
alternative exists.
