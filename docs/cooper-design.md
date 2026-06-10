# apt-cooper — Design

## Problem

Plenty of useful tooling on GitHub doesn't ship a `.deb` at all — projects
release a static `linux_amd64` binary, or a `*.tar.gz` containing `bin/`,
`share/man/`, `etc/`. Getting those onto Debian boxes through apt means
somebody has to package them.

Hand-rolling `nfpm.yaml` per project is tractable but tedious: each
package needs the same per-arch download URL templating, the same
release-tag → version mapping, and a re-pointing every time upstream cuts
a release.

## Approach

apt-cooper is a two-phase tool with a JSON contract between them:

```
       YAML config                              JSON plan                 .deb files
            │                                       │                         │
            ▼                                       ▼                         ▼
      ┌──────────┐  GitHub releases API     ┌──────────┐ asset downloads ┌──────────┐
      │ discover │ ───────────────────────► │ JSON plan│ ──────────────► │  build   │
      └──────────┘                          └──────────┘                 └──────────┘
                                                  │
                                                  ▼
                                            orchestrator (out of scope):
                                            queries target repo, drops
                                            duplicates and version
                                            regressions
```

`cooper discover <CONFIG>` reads YAML config (a top-level `cooper.yaml`
plus one multi-doc YAML file per package — doc 1 cooper-shaped, doc 2
**vanilla nfpm**), resolves the GitHub release per package, and emits a
JSON plan to stdout. `cooper build <JSON_FILE>` reads the plan, downloads
assets, stages files at the paths doc 2 references, and invokes nfpm
**as a library** (`github.com/goreleaser/nfpm/v2`) per artifact to
emit `.deb`s. The JSON between phases is the orchestration boundary
**and** the plugin point: any program emitting valid JSON is a
producer (see *External producers*).

The trivial pipeline `cooper discover c.yaml | cooper build -` is the
no-orchestrator case — always builds whatever discover found, at the
bare version with no revision. Dedup and version policy live in the
orchestrator; see *Orchestrator dedup & version policy*.

## Output & downstream publishing

Cooper's outputs are `.deb` files in `--out-dir` plus an annotated
copy of the input JSON on stdout. Pushing those into a hosted repo
is **out of scope** — a separate publisher tool (`aptly`/`reprepro`
wrapper, S3 sync, `dpkg-scanpackages`, etc.) consumes the output
directory and JSON. Cooper emits **unsigned** `.deb`s; per-`.deb`
signing (`debsigs`) is the repo manager's concern.

The contract cooper offers downstream:

- Predictable filename `<name>_<version>_<arch>.deb`.
- `build_inputs_hash` per artifact, stable across runs (see
  *Reproducibility*) — embedded into each `.deb` as the
  `X-Cooper-Build-Inputs-Hash` control field for orchestrator-side
  dedup (see *Orchestrator dedup & version policy*).
- Process exit code 0 only when every requested artifact built cleanly.

## Orchestrator dedup & version policy

Cooper never reaches into the target apt repository. Dedup and
version policy live in the orchestrator — typically a thin wrapper
around `aptly`, `reprepro`, or whichever repo manager owns the
published tree.

### Primary dedup key: `X-Cooper-Build-Inputs-Hash`

Every `.deb` cooper produces carries an `X-Cooper-Build-Inputs-Hash`
control field whose value is the artifact's `build_inputs_hash`
verbatim (`"sha256:<hex>"`). Cooper-build injects it as one of
three on-disk-only adjustments at `nfpm.yaml`-write time (see
*Build*); it does not flow into `build_plan.nfpm`, so its presence
doesn't self-reference into the hash. dpkg, apt, and aptly preserve
unknown control fields verbatim and (in aptly's case) index them
for `?q=` queries.

Orchestrators use this field as the primary dedup test, one query
per artifact in the plan:

| Hash present in repo? | Action                                                |
| --------------------- | ----------------------------------------------------- |
| Yes                   | Skip — idempotent re-discovery                        |
| No                    | Build and publish (see *Auto-bump* for version policy)|

Because the hash excludes the orchestrator's revision choice (see
*`--revision N` mechanics*), re-running discover after a revision
bump still hits the skip branch.

### Auto-bump: resolving a hash miss

When the hash isn't in the repo, the orchestrator queries by
`(Package, base-version, Architecture)` to pick a debian-revision
that doesn't collide:

| `(Package, base-version, Architecture)` in repo? | Publish at                                       |
| ------------------------------------------------ | ------------------------------------------------ |
| Absent                                           | Bare version (`1.0.0`)                           |
| Present, max revision = N                        | `<bare>-(N+1)` (`1.0.0-1`, `1.0.0-2`, ...)       |

wget's `filename.1` pattern lifted to apt. Recipe edits, upstream
tag mutations (asset SHA changes under the same release), and
`format_revision` bumps all flow through this path because each
perturbs `build_inputs_hash` (see *`build_inputs_hash` inputs*).
Bare (no debian-revision) counts as N=0 for the `(N+1)` formula, so
publishing on top of a bare-only `(Package, base-version, Architecture)`
yields `<bare>-1`. The orchestrator passes `N+1` to cooper via
`cooper build --revision N+1 <plan.json>`; `pkg/plan` exposes
`SplitDebianRevision` and `CompareVersions` so every implementation
parses Debian versions the same way.

### `--revision N` mechanics

`cooper build --revision N` sets nfpm's `release: N` field on every
artifact's on-disk `nfpm.yaml` — one of the on-disk-only adjustments
alongside `expand: true`, the `X-Cooper-Build-Inputs-Hash` injection,
and the `version_schema: none` default. nfpm's deb packager
concatenates `<version>-<release>` into the resulting Debian Version,
so the published `.deb`'s version is `<bare>-N` and its filename is
`<name>_<bare>-<N>_<arch>.deb`. **The `build_inputs_hash` is
unchanged.** The revision is post-discover metadata, not a build
input; including it would mean re-discovery never hits the dedup
branch (infinite re-bumping).

Cooper uses nfpm's dedicated `release:` field rather than rewriting
`version:` so the JSON `build_plan.nfpm.version` stays revision-free
and the same Plan re-fed through `cooper build --revision` produces
the bumped `.deb` deterministically without the discover-time hash
needing to know the revision in advance. (The `version_schema: none`
pin from step 4 already keeps nfpm from rewriting either the bare
version or any `-N` suffix on its own; `release:` is the structural
separation, not just a quirk workaround.)

`build_plan.nfpm.version` in the JSON stays bare; `cooper discover`
is revision-unaware (no `--revision` flag, never emits `release:` or
`-N`). `--revision N` errors when a recipe has already populated the
debian-revision slot — either as `-` in `version:` (a baked-in
revision under `version_schema: none`, or a semver prerelease that
would conflict) or as a non-empty `release:` field in doc 2. Re-fed
annotated plans (those with `deb.path` populated) are rejected
unconditionally — see *Build*.

### Version monotonicity

apt clients upgrade only when a repo candidate sorts strictly newer
than the installed version per `dpkg --compare-versions`
(Policy §5.6.12). Publishing an older version of a package whose
newer version already exists in the repo is silently a no-op for
clients. Orchestrators MUST filter discover output against the
repo's current "highest version per `(Package, Architecture)`" set
and drop any artifact whose version sorts strictly older than that
maximum. The shared comparator lives in `pkg/plan` (`CompareVersions`).

### Audit & safety

These are orchestrator concerns — cooper has no logging contract or
strict-mode flag, since it doesn't make publish decisions.
Orchestrator implementations should:

- Log every auto-bump with prior hash, new hash, prior revision,
  new revision.
- Expose a strict-mode flag for environments that require human
  gating; the default workflow is auto-bump.
- Alert on revision counts per
  `(Package, base-version, Architecture)` exceeding a threshold,
  catching runaway rebuilds.

## Config

### Top-level `cooper.yaml`

```yaml
github:
  token_env: GITHUB_TOKEN              # for higher rate limits
  # token_file: /var/lib/cooper/secrets/gh.token   # alternative

packages:
  - ./packages/hugo.yaml
  - ./packages/terraform.yaml
```

`packages:` is a list of paths to per-package multi-doc files. Debian
package names come from each file's nfpm document (`name:`); not
duplicated here.

There is no top-level `gitea:` block. Each `source.gitea` sidecar names
its own server URL (`source.gitea.server`) and, optionally, its own
`token_env` / `token_file` — recipes commonly target multiple distinct
Gitea instances, each with its own auth.

There are no `paths:` keys — output and work directories are CLI flags
on `cooper build` (see *CLI*), not in `cooper.yaml`. They're build-time
concerns, and discover and build can run on different hosts.

### Per-package multi-doc file

N YAML documents (N≥2) in one file, separated by `---`:

- **Doc 1** is the cooper sidecar — source resolution shared across
  every `.deb` the recipe produces.
- **Docs 2..N** are each a vanilla nfpm config — one per produced
  `.deb`. Each becomes its own `plan.Package` entry with its own
  `build_inputs_hash`, dedup'd and revision-bumped independently by
  drayman.

Strict ordering — no `kind:` discriminator, since putting one in
nfpm docs would break the "vanilla nfpm" property. The N>=2 shape
lets a single source resolution fan out into multiple `.deb`s
(`ollama` + `libollama-nvidia` from one `ollama-linux-<arch>.tar.zst`
release, `cfssl` + `cfssljson` + `mkbundle` from one cloudflare/cfssl
release, …) without duplicating the source block across recipe
files. The two-doc form is the singleton case.

> **Cross-source bundling is expressly out of scope.** All nfpm docs
> in one recipe share the same source resolution and the same staged
> asset set. Producing one `.deb` with files from multiple unrelated
> upstreams isn't supported — each `.deb`'s reproducibility and
> dedup semantics rely on exactly one upstream provenance.

```yaml
# packages/hugo.yaml

---
# Doc 1 — cooper sidecar
source:
  github:
    repo: gohugoio/hugo
    release: latest                    # or: { tag_pattern: "^v\\d+\\.\\d+\\.\\d+$" }
    include_prerelease: false

version_from: tag_strip_v              # tag | tag_strip_v | asset_filename | fixed
# version_template: "{tag_strip_v}+ds1"  # optional override
# epoch: 0                               # optional, for downgrade-recovery only

arches:
  amd64: { asset: "hugo_extended_${VERSION}_linux-amd64.tar.gz" }
  arm64: { asset: "hugo_extended_${VERSION}_linux-arm64.tar.gz" }

---
# Doc 2 — vanilla nfpm
name: hugo
version:  ${VERSION}
arch:     ${ARCH}
platform: linux
maintainer: Ophymx <ops@ophymx.com>
description: A fast and flexible static site generator built with love.
homepage: https://gohugo.io
license:  Apache-2.0
section:  utils
contents:
  - src: ${ASSETS}/hugo
    dst: /usr/bin/hugo
    file_info: { mode: 0755 }
  - src: ./hugo.service                # cooper stages from ./hugo.service or ./hugo.service.tmpl
    dst: /lib/systemd/system/hugo.service
    file_info: { mode: 0644 }
  - src: ./completions/*               # cooper expands; stages every match
    dst: /usr/share/bash-completion/completions/
scripts:
  postinstall: ./hugo-postinstall.sh   # cooper stages with .tmpl fallback
deb:
  fields:
    Bugs: https://github.com/gohugoio/hugo/issues
```

Cooper validates: at least two documents; doc 1 has `source:`;
docs 2..N each have a `name:`, and names are unique within the
recipe. Anything else is a hard config error.

### Doc 1 schema

```
source.github.repo            <owner>/<name>           required
source.github.release         "latest"                 default
                              { tag_pattern: REGEX }
                              { tag: STRING }
source.github.include_prerelease  bool                 default false
source.github.source_archive  bool                     default false
                              (also fetch
                              archive/refs/tags/<tag>.tar.gz;
                              arches[].asset becomes optional)
source.gitea.server           URL                      required
                              (e.g. https://gitea.example.com)
source.gitea.repo             <owner>/<name>           required
source.gitea.release          (same union as           default "latest"
                               source.github.release)
source.gitea.include_prerelease  bool                  default false
source.gitea.source_archive   bool                     default false
                              (uses Release.tarball_url
                              from the Gitea API)
source.gitea.token_env        STRING                   optional (auth)
source.gitea.token_file       PATH (absolute, 0400)    optional (auth)
source.external.command       [STRING, ...]            required (argv)
source.external.env           map<STRING, STRING>      optional (literal)
source.external.env_forward   [STRING, ...]            optional; forward
                                                       caller env if set
source.external.timeout       DURATION                 default 30s
version_from                  tag | tag_strip_v | asset_filename | fixed
                              (rejected when source.external is set;
                              version comes from the script's .version)
version_template              STRING                   optional
                              (rejected when source.external is set)
version_regex                 STRING                   required when
                                                       version_from == asset_filename
                              (rejected when source.external is set)
epoch                         int                      default 0
arches                        map<arch, {
                                  asset: STRING        singular (sugar; one asset)
                                  assets: [STRING]     plural (multi-asset)
                                  vars: map<STRING,    optional; per-arch
                                          STRING>      ${KEY} substitutions
                                                       into doc 2
                              }>                       ≥ 1 entry
extract.max_bytes             STRING (humanized)       optional; default 1 GiB
                              ("8GiB", "4096MB", or    (ceiling 64 GiB)
                              bare integer bytes)
extract.max_files             int                      optional; default 100k
                                                       (ceiling 2,000,000)
```

Doc 1's only string-substitution surface is the `arches.<arch>.asset(s):`
value(s). The single legal substitution is `${VERSION}` (uppercase, same
syntax as doc 2; resolved per *Version selection* below). After
substitution, each selector is matched for **exact equality** against
the release's asset names. Zero or more-than-one matches → discover
error.

Singular `asset:` is syntactic sugar for a single-entry `assets:` list;
recipes pick whichever reads better. Validation rejects setting both
on the same arch entry. The plural form covers cases like cfssl, where
a single release ships N independent binaries with no bundling archive
— each binary's selector becomes one `assets:` entry and the staged
files land side-by-side under `${ASSETS}/`. Asset name collisions
(two selectors resolving to the same release-asset name) are rejected
at discover time.

### Cooper's substitution into doc 2

Cooper substitutes a fixed set of variables into doc 2; anything
else passes through verbatim.

| Variable        | Resolved by | Where in JSON                                    |
| --------------- | ----------- | ------------------------------------------------ |
| `${VERSION}`    | discover    | substituted to a literal in `build_plan.nfpm`    |
| `${ARCH}`       | discover    | substituted to a literal; Debian arch (`amd64`, `arm64`, `armhf`, `riscv64`, …) |
| `${ARCH_GNU}`   | discover    | substituted to a literal; GNU/uname convention (`x86_64`, `aarch64`, `armv7l`, `riscv64`) — derived from `${ARCH}` via the table below |
| `${<KEY>}`      | discover    | from `arches[<arch>].vars` (see *Per-arch user variables*); substituted to a literal in `build_plan.nfpm` |
| `${ASSETS}`     | build       | left symbolic; build supplies it through nfpm.ParseWithEnvMapping's env mapper |
| `${SOURCE}`     | build       | symbolic; supplied through the env mapper only when the artifact carries a `source_archive` (otherwise unset — recipes that reference `${SOURCE}` without enabling source_archive will fail nfpm expansion) |

For nfpm to expand `${ASSETS}` / `${SOURCE}` at build time, every
`contents[]` entry needs `expand: true`. Cooper sets that flag
(preserving any explicit user value) when writing the on-disk
`nfpm.yaml`; see *Build* step 4 for the full set of on-disk-only
adjustments.

The discover-substituted names above are the full set cooper resolves
into doc-2 literals. There is no `${AUX}`, no `${TMPL_*}`. The only
build-time symbolic names are `${ASSETS}` and `${SOURCE}`.

#### Built-in arch aliases

`${ARCH_GNU}` resolves via a fixed table cooper applies at discover
time, after `${ARCH}` is known:

| `${ARCH}` | `${ARCH_GNU}` |
| --------- | ------------- |
| `amd64`   | `x86_64`      |
| `arm64`   | `aarch64`     |
| `armhf`   | `armv7l`      |
| `riscv64` | `riscv64`     |

The table is the contract — adding rows is non-breaking; renaming or
removing a row bumps `format_revision` because resolved
`build_plan.nfpm` bytes would diverge. Recipes targeting an arch
outside the table that reference `${ARCH_GNU}` in doc 2 fail at
discover with a dedicated `arch_gnu_unknown` error (distinct from
the generic unresolved-substitution kind). The message must:

- name the recipe, the arch key, and the doc-2 field where the
  reference appeared (e.g. `contents[2].src`),
- print the full `${ARCH}` → `${ARCH_GNU}` mapping table verbatim so
  the user can see which arches are covered without having to
  consult the docs,
- spell out the two fixes: drop the `${ARCH_GNU}` reference from
  doc 2, or pick a different per-arch `vars` key (e.g.
  `TARBALL_ARCH`) for the literal the recipe needs. `vars` cannot
  shadow `ARCH_GNU` itself per the reserved-name rule, so
  redefining the built-in for an unsupported arch isn't an option —
  by design, since adding arches to the table is the contract
  surface for new arches. Validate emits the same message offline.

Cooper does not synthesize a fallback; silent empty-string
substitution would let a bad recipe ship a `.deb` with the wrong
internal layout under a working hash.

Rationale for shipping `${ARCH_GNU}` as a built-in: the
`x86_64`/`aarch64` spelling shows up inside vendor tarballs (and
sometimes in asset filenames) often enough that asking every recipe
to redefine it via `vars:` is busywork. Other naming conventions
(`x64`, `armv7`, `rv64`, microarch tiers, full GNU triples with
vendor/OS suffixes) are vendor-specific and stay in `vars:`.

#### Per-arch user variables (`arches[].vars`)

For naming the built-in aliases don't cover, each arch entry takes an
optional `vars:` map of literal strings exposed as `${KEY}`
substitutions in doc 2:

```yaml
arches:
  amd64:
    asset: "tool-linux-x86_64.tar.gz"
    vars:
      TARBALL_DIR: x86_64-unknown-linux-gnu
      VENDOR_CODE: x64
  arm64:
    asset: "tool-linux-aarch64.tar.gz"
    vars:
      TARBALL_DIR: aarch64-unknown-linux-gnu
      VENDOR_CODE: arm64
```

Doc 2 references the keys like any other substitution:

```yaml
contents:
  - src: ${ASSETS}/${TARBALL_DIR}/bin/tool
    dst: /usr/bin/tool
```

Semantics:

- **Discover-time substitution.** Each `${KEY}` is replaced with the
  literal value at discover time, identical to how `${VERSION}` /
  `${ARCH}` / `${ARCH_GNU}` resolve. The substituted literal lives in
  `build_plan.nfpm`; nfpm never sees the variable name and the
  build-time env scrub stays unchanged.
- **Reserved keys.** `VERSION`, `ARCH`, `ARCH_GNU`, `ASSETS`,
  `SOURCE`, and any future cooper-defined substitution name are
  rejected at validate time — `vars:` cannot shadow a built-in. The
  keyspace is otherwise open.
- **Key grammar.** `[A-Z][A-Z0-9_]*` — uppercase to match the other
  substitution names and avoid collision with arbitrary text in doc
  2. Lowercase or punctuation-bearing keys are validate errors.
- **Per-arch independence.** The set of keys may differ across arches
  (e.g. only the `amd64` entry sets `MICROARCH_TIER`). Referencing an
  unset key from doc 2 for that arch is a discover error per the
  unresolved-substitution rule; cooper does not invent empty-string
  defaults.
- **Reproducibility.** Vars feed `build_inputs_hash` via the resolved
  `build_plan.nfpm` — no separate hash input, no `schema_version`
  bump. Changing a `vars` value flips the hash naturally because the
  substituted literal flips inside `build_plan.nfpm`.
- **External producers.** External producers (see *External
  producers*) get this surface for free — they emit pre-substituted
  `build_plan.nfpm` and never see `${KEY}` syntax. The contract
  belongs to discover.

### Aux file resolution (and templating)

For two — and **only two** — sections of doc 2, cooper makes sure files
referenced by relative path actually exist when nfpm runs:

1. `contents[].src`
2. `scripts.preinstall`, `scripts.postinstall`, `scripts.preremove`,
   `scripts.postremove`

Every other field — `name`, `description`, `depends`, `changelog`,
`deb.fields.*`, `deb.signature.*`, `rpm.*`, `apk.*`, anything else —
is **passthrough**. Cooper does not stage files for those fields,
and within the in-scope sections cooper rewrites relative `src:`
paths to be rooted at the staging directory before nfpm reads them.
A passthrough relative path like `changelog: ./CHANGELOG.md` will
not resolve unless the user uses an absolute path; cooper doesn't
touch these fields.

Within the in-scope sections, cooper classifies each path by shape and
acts:

| Shape                   | Identification                                | Action                                                                              |
| ----------------------- | --------------------------------------------- | ----------------------------------------------------------------------------------- |
| Absolute                | starts with `/`                               | passthrough                                                                         |
| `${VAR}/...`-prefixed   | starts with `${`                              | passthrough                                                                         |
| Glob                    | contains `*`, `?`, or `[…]`                  | expand against package directory; inline each match                                 |
| Directory               | resolves to a directory on disk               | recursively stage every file under it                                               |
| Literal file            | resolves to a regular file (or has `.tmpl` sibling) | inline the file's bytes; if file is missing but `<P>.tmpl` exists, render and inline at `<P>` |

For literal-file paths, the render-or-copy rule has these hard errors
at discover:

- Both `<P>` and `<P>.tmpl` exist (ambiguous; user deletes one).
- Neither `<P>` nor `<P>.tmpl` exists.
- Glob match ends in `.tmpl` (cooper will not silently ship an
  unrendered template).
- Directory walk surfaces a `.tmpl` (same reasoning).
- Glob with zero matches.
- Any path resolves outside the package multi-doc file's directory
  after `..` and symlink resolution.

`aux_files` keys are the source-relative paths the user wrote in doc 2
(e.g. `./hugo.service`, `./completions/hugo.bash`), so different paths
with the same basename coexist as distinct keys.

Cooper uses `github.com/bmatcuk/doublestar/v4` (pinned to the same
version nfpm uses) for glob expansion.

**Doc 2 stays vanilla nfpm.** The user's `src: ./hugo.service` is the
exact string nfpm sees. If they stage the files manually (or via
`cooper build --stage-only`), `nfpm pkg -f doc2.yaml` works directly.
If files aren't staged, nfpm errors loudly with "no such file."

#### Templates

`.tmpl` files are rendered with Go `text/template`. Closed variable
set:

| Variable        | Type   | Value                                |
| --------------- | ------ | ------------------------------------ |
| `.Name`         | string | package name (from doc 2 `name:`)    |
| `.Version`      | string | resolved version                     |
| `.Arch`         | string | per-arch key                         |
| `.Epoch`        | int    | package epoch (default 0)            |
| `.PublishedAt`  | string | release `published_at` (ISO 8601)    |

No `funcMap`, no `now`, no env access. The set is a one-way door —
every entry becomes part of `build_inputs_hash` once shipped.

Each arch renders independently (same template, different `.Arch`),
producing per-arch rendered bytes → per-arch `aux_files` → per-arch
`build_inputs_hash`.

### Version selection

Cooper computes the `${VERSION}` value once per package per discover
run:

| `version_from`   | Source                                                   |
| ---------------- | -------------------------------------------------------- |
| `tag`            | release tag verbatim                                     |
| `tag_strip_v`    | tag with leading `v` stripped (default; most common)     |
| `asset_filename` | match `version_regex` against the matched asset's name; capture groups become available substitutions |
| `fixed`          | literal string in `version_template`                     |

`version_template` is a final `{...}`-substituted string applied after
`version_from`. Available substitutions: `{tag}`, `{tag_strip_v}`,
`{date}` (UTC `YYYYMMDD` derived from `release.published_at`), and
named regex groups from `version_regex` when
`version_from: asset_filename`. Covers `{tag_strip_v}+ds1` etc.
without a full templating language.

If the resulting version doesn't match Debian's
`^[0-9][A-Za-z0-9.+~-]*$`, discover errors.

## Phases

### Discover

```
load top-level cooper.yaml.
for each package file in cooper.yaml.packages:
  1. Parse multi-doc file: validate two-doc structure, doc 1 + doc 2 schemas.
  2. Hit /repos/<owner>/<name>/releases/latest (or list+filter for
     tag_pattern / include_prerelease).
  3. Compute version string from version_from + version_template.
  4. For each arch in doc 1's arches:
       a. Substitute ${VERSION} into the asset selector; find the unique
          matching release asset. Read URL, size, GitHub-supplied SHA256.
          Do not download.
       b. Substitute ${VERSION}, ${ARCH}, ${ARCH_GNU}, and any
          arches[<arch>].vars keys into doc 2 → resolved nfpm config
          (per arch). Unresolved ${KEY} → discover error.
       c. Walk doc 2's contents[].src and scripts.* fields per the
          *Aux file resolution* rules; inline bytes (rendering .tmpl as
          needed) into aux_files keyed by the source-relative path the
          user wrote.
       d. Compute build_inputs_hash for this artifact.
       e. Emit one artifacts[] entry.
  5. Emit one packages[] entry (or a result: "error" entry if any
     sub-step failed).
emit one JSON document on stdout.
```

Discover does not modify any cooper-owned state. A failed package
becomes an `error` entry; it does not abort the run. Network use is
bounded — one GitHub releases API call per package, plus optional
`HEAD` per asset when SHA256 isn't on the release payload. The
algorithm above is the github_release path; gitea_release, json_url,
xml_url, and external substitute their own discovery step (Gitea
SDK call, HTTP GET + gjson, HTTP GET + XPath, and child-process exec
respectively) at step 2 but otherwise follow the same shape. The
gitea_release path additionally requires `source.gitea.server` so
cooper knows which Gitea instance to call, and skips SHA256 on
attachments (Gitea's API does not expose a digest) — build streams
and hashes at download time as it already does for json_url. External-
source recipes exec a user-supplied script; cooper sandboxes it but
the script's own side effects are the script author's responsibility.

### Build

```
parse JSON plan.
reject plans whose artifacts have deb.path populated (re-fed
  annotated build output is not valid input).
verify build_inputs_hash on every artifact (rejects tampered/stale
  plans). The hash is computed once at discover and never recomputed —
  including by --revision (see *Orchestrator dedup & version policy*).
if --revision N is set:
  for each artifact with result: "ok":
    if build_plan.nfpm.version contains "-", error. Otherwise the
    revision is applied in step 4 below as an on-disk-only mutation;
    build_plan.nfpm.version in the JSON stays bare.
for each artifact (across packages[*].artifacts[*]) with result: "ok":
  staging = <work-dir>/<run-id>/<package-name>/<arch>/
        # <run-id> is a per-invocation random hex id; never appears in the .deb.
  1. Stream-download asset to <staging>/asset/<asset.name>;
     verify size + SHA256.
  2. If the asset is an archive (detected by extension: .tar.gz / .tgz /
     .tar.xz / .txz / .tar.zst / .tar.bz2 / .zip), unpack it into
     <staging>/asset/ and remove the archive file. No prefix stripping —
     a top-level dir in the archive becomes a top-level dir under
     ${ASSETS}/. Anything else (raw binary, single file) is left at
     <staging>/asset/<asset.name>; the user references it as
     ${ASSETS}/<asset.name>.
  3. Materialize aux_files: for each (key, content) in build_plan.aux_files,
     write content to <staging>/<key> (preserving the source-relative path).
  4. Write resolved nfpm.yaml to <staging>/nfpm.yaml from
     build_plan.nfpm with four on-disk-only adjustments:
       (a) expand: true on every contents[] entry whose user did not
           explicitly set the flag, so nfpm expands ${ASSETS} at exec
           time.
       (b) deb.fields["X-Cooper-Build-Inputs-Hash"] =
           build_inputs_hash (already "sha256:<hex>" in the JSON), so
           orchestrators and repo managers can query the published
           .deb (see *Orchestrator dedup & version policy*).
       (c) if --revision N is set: write release: "N" alongside the
           bare version (the JSON stays bare; nfpm concatenates as
           "<version>-<release>" at build time).
       (d) version_schema: none when the recipe didn't explicitly
           choose one. nfpm's default schema is semver, which pads
           short versions (`4.32` → `4.32.0`) and rewrites a "-N"
           suffix as the Debian tilde-prerelease ("~N"). Either
           transform makes the deb's `Version:` field disagree with
           the version cooper resolved — and with the filename cooper
           wrote — breaking orchestrator dedup queries. Pinning
           `none` makes nfpm emit the version verbatim.
     None of these adjustments flow back into the JSON, so none of
     them perturb build_inputs_hash.
  5. Invoke nfpm/v2 in-process to write the .deb to
     <out-dir>/<artifact.deb.filename>:
       a. nfpm.ParseWithEnvMapping reads <staging>/nfpm.yaml with a
          cooper-controlled env-mapping function. Only these names
          resolve; anything else returns "" (same behavior as the
          prior exec path running under a scrubbed env):
            VERSION=<from JSON>     ARCH=<from JSON>
            ASSETS=<staging>/asset  SOURCE_DATE_EPOCH=<from JSON>
            SOURCE=<staging>/source  (omitted when no source_archive)
       b. config.Get("deb") materializes the nfpm.Info struct.
       c. info.MTime is pinned to time.Unix(source_date_epoch, 0).UTC()
          so nfpm.WithDefaults can't fall back to modtime.FromEnv()
          reading the process SOURCE_DATE_EPOCH (which cooper does
          not set on its own process).
       d. Relative `src:` paths in info.Contents are rewritten to be
          rooted at <staging>; nfpm's os.Stat would otherwise resolve
          them against the build process's cwd. (Absolute paths from
          ${ASSETS}/${SOURCE} expansion are left untouched.)
       e. nfpm.Validate(info) → packager.Package(info, outFile).
  6. SHA256 the resulting .deb.
re-emit annotated JSON on stdout (artifact.deb.path / .sha256 populated).
clean <work-dir>/<run-id>/ (unless --keep-work).
```

Build refuses entries with `result: "error"` (orchestrator drops them
before piping). The hash-verification preamble runs exactly once,
against the incoming plan; the value it asserts is the value embedded
into the `.deb` and re-emitted in the annotated JSON, regardless of
whether `--revision` is in play (see *Orchestrator dedup & version
policy*).

Failures isolate per-artifact; a failed `foo amd64` does not block
`bar amd64`. The re-emitted JSON marks failed artifacts
`result: "error"` with `{"kind": "build_failed", "message": "<text>"}`.
Process exit code is non-zero whenever any artifact failed.

Multi-artifact fetches share an in-process per-run cache keyed by
`asset.url + asset.sha256`, so two artifacts referencing the same
archive download once.

## Discover JSON contract

The JSON emitted by discover is the boundary cooper presents to any
orchestrator, publisher, or repo-query tool. Versioned, additive,
small enough to read by eye.

```json
{
  "schema_version": 1,
  "tool": { "name": "cooper", "version": "0.1.0", "format_revision": 1 },
  "discovered_at": "2026-05-10T12:34:56Z",
  "packages": [
    {
      "name": "hugo",
      "result": "ok",
      "source": {
        "kind": "github_release",
        "repo": "gohugoio/hugo",
        "release_id": 178213984,
        "release_tag": "v0.140.0",
        "release_published_at": "2026-05-09T08:12:00Z"
      },
      "artifacts": [
        {
          "arch": "amd64",
          "assets": [{
            "name":   "hugo_extended_0.140.0_linux-amd64.tar.gz",
            "url":    "https://github.com/.../linux-amd64.tar.gz",
            "size":   19283746,
            "sha256": "sha256:abc123...",
            "sha256_source": "github_api"
          }],
          "deb": {
            "filename":          "hugo_0.140.0_amd64.deb",
            "build_inputs_hash": "sha256:deadbeef...",
            "path":   null,
            "sha256": null
          },
          "build_plan": {
            "source_date_epoch": 1715240520,
            "nfpm": {
              "name": "hugo",
              "version": "0.140.0",
              "arch": "amd64",
              "platform": "linux",
              "maintainer": "Ophymx <ops@ophymx.com>",
              "description": "A fast and flexible static site generator built with love.",
              "homepage": "https://gohugo.io",
              "license": "Apache-2.0",
              "section": "utils",
              "contents": [
                { "src": "${ASSETS}/hugo",  "dst": "/usr/bin/hugo",                    "file_info": { "mode": 493 } },
                { "src": "./hugo.service", "dst": "/lib/systemd/system/hugo.service", "file_info": { "mode": 420 } }
              ],
              "scripts": { "postinstall": "./hugo-postinstall.sh" },
              "deb": { "fields": { "Bugs": "https://github.com/gohugoio/hugo/issues" } }
            },
            "aux_files": {
              "./hugo.service":        { "content_b64": "..." },
              "./hugo-postinstall.sh": { "content_b64": "..." }
            }
          }
        },
        { "arch": "arm64", "assets": [ { "...": "..." } ], "deb": { "...": "..." }, "build_plan": { "...": "..." } }
      ]
    },
    {
      "name": "broken",
      "result": "error",
      "error": { "kind": "discovery_failed", "message": "GET https://api.github.com/repos/owner/repo/releases/latest: 404" }
    }
  ]
}
```

`build_plan` is **per artifact**, not per package — it includes
per-arch substitutions in `nfpm.arch` and per-arch rendered template
bytes in `aux_files`. (Multi-arch packages duplicate most of
`build_plan` across artifacts; the duplication is intentional — it
keeps the schema flat and `build_inputs_hash` per-artifact natural.)

Field notes:

- **`tool.format_revision`** is a small integer cooper bumps when its
  output `.deb` bytes would diverge for the same logical inputs (nfpm
  major upgrade, gzip stdlib change). Independent of `tool.version`.
- **`asset.sha256`** is `"sha256:<hex>"`. `sha256_source` is
  `github_api`, `head_request`, or `null`. If `null`, build computes
  the hash during streaming download and records it in the re-emitted
  JSON; the orchestrator should treat such artifacts as "re-import
  unconditionally" because dedup needs the SHA in `build_inputs_hash`.
  `gitea_release` always lands here (Gitea attachments expose no
  digest); the json_url / xml_url / external paths land here whenever
  the producer can't supply a SHA at discover time.
- **`build_plan.nfpm`** is doc 2 with every discover-time
  substitution (`${VERSION}`, `${ARCH}`, `${ARCH_GNU}`, and any
  user-defined `arches[<arch>].vars` keys) resolved to literals;
  `${ASSETS}` and `${SOURCE}` left symbolic. Globs and
  directory references are also kept verbatim (e.g.
  `src: ./completions/*` is unchanged) — nfpm itself re-expands at
  build time against the staged tree. Cooper expands at discover only
  to determine which files to inline into `aux_files`, not to rewrite
  doc 2's `contents[]`. Build emits this subtree byte-for-byte as the
  on-disk `nfpm.yaml`. (File-info modes are decimal in JSON: `0755` →
  `493`, `0644` → `420`.)
- **`build_plan.aux_files`** is a map from source-relative paths to
  base64 bytes. For literal-file references and `.tmpl` renders, the
  key is the path doc 2 wrote (`./hugo.service`). For glob and
  directory references, one key per inlined match
  (`./completions/hugo.bash`, `./completions/hugo.zsh`, …). Build
  materializes each at `<staging>/<key>` so doc 2's relative-path
  references resolve against the staging dir (build absolutizes
  relative `src:` paths in `info.Contents` before handing the doc to
  nfpm). Raw inlined files and rendered templates are indistinguishable
  here — intentional.
- **`deb.path` / `deb.sha256`** are `null` in discover output and
  populated in build output. Same JSON shape on both sides.
- **`artifact.source_archive`** is an optional sibling of `assets[]`
  populated when the recipe sets `source.github.source_archive: true`.
  Shape mirrors `asset` (name, url, size, sha256, sha256_source);
  the URL is the auto-composed
  `https://github.com/<repo>/archive/refs/tags/<tag>.tar.gz`. SHA256
  is `null` at discover time (the API doesn't expose a digest for
  archive URLs); build streams + hashes during download, records the
  value in the re-emitted JSON, and stages the extracted tree under a
  per-artifact `source/` dir whose absolute path is supplied to nfpm
  through the env mapper as `${SOURCE}`.
- **`artifact.extract`** is an optional sibling of `build_plan` with
  `max_bytes` (int64) and `max_files` (int); absent means "use cooper's
  defaults" (1 GiB / 100 000). Populated by discover from the recipe's
  `extract:` block; external producers may set it directly. Lives
  outside `build_plan` so it doesn't enter `build_inputs_hash` —
  two builds with different extract caps but the same logical inputs
  still produce byte-identical `.deb`s.
- **Artifact-level `result` / `error`** are absent on discover output
  (artifact failures collapse into a package-level error there) and
  populated by build only when an individual artifact's pipeline
  failed. Sibling artifacts in the same package finish regardless. An
  empty `result` on an artifact means "ok" — the field uses
  `omitempty`.
- **`error.kind`** values: `discovery_failed` (GitHub API or asset
  resolution); `version_invalid` (resolved version doesn't match
  Debian's grammar); `aux_resolution_failed` (template render error,
  missing file, glob with no matches, etc.); `unresolved_substitution`
  (doc 2 references a `${KEY}` that's neither a built-in nor declared
  in `arches[<arch>].vars`); `arch_gnu_unknown` (doc 2 references
  `${ARCH_GNU}` for an arch cooper has no GNU mapping for; message
  prints the supported table); `build_failed` (build-only; nfpm exec,
  asset SHA mismatch, archive sandbox rejection, or any other
  build-pipeline failure).
- **Filtering**: orchestrators may drop entire `packages[]` or
  `artifacts[]` entries. They MUST NOT edit any other field — build
  re-checks `build_inputs_hash` and rejects mismatches.
- **Schema additions** are non-breaking; unknown fields are ignored.
  Removals or semantic changes bump `schema_version`.
- **JCS canonicalization** (RFC 8785) is used for any subtree that
  feeds into a hash.

### `build_inputs_hash` inputs

Computed once per artifact at discover time, never recomputed.
The orchestrator's primary dedup key (see *Orchestrator dedup &
version policy*).

```
build_inputs_hash = SHA256(JCS({
    "format_revision": tool.format_revision,
    "asset_sha256s":   [artifacts[i].assets[*].sha256],
    "build_plan":      artifacts[i].build_plan
}))
```

`asset_sha256s` is the per-asset SHA list in artifact order — the
same order `Assets` appears in the plan. A `null` entry means
"unknown at discover time; build streams + hashes." An empty list is
the staves / asset-optional case; the canonical form distinguishes an
empty array `[]` from a missing/null slice.

That's the full list of inputs. `build_plan` already carries
everything else: the resolved nfpm config (with name/version/arch/epoch
substituted to literals), every aux_files entry, `source_date_epoch`.
Including `(name, version, epoch, arch)` separately would double-count.

The non-obvious exclusions, with rationale:

- `discovered_at` — wall-clock; non-deterministic.
- `tool.version` — moves on every release; `format_revision` is the
  explicit signal for output divergence.
- `source.*` fields — pure provenance. `release_published_at`
  specifically is excluded because it already flowed into
  `source_date_epoch`; including it raw would make the hash drift if
  cooper ever changed how it derives the epoch.
- `asset.url` — can change (mirror, redirect) without the bytes
  changing.

The included/excluded set is part of the public contract. Changing it
is a hard schema change; additions or removals require bumping
`schema_version` and keeping prior `format_revision`s on the old
algorithm for one release cycle.

## External producers

> Most cases that previously reached for this pattern are now covered
> by the `external` source kind (see *Source kinds → External source
> script contract*) — the source-kind path leaves cooper in charge of
> `build_inputs_hash`, `source_date_epoch`, aux_files, and BuildPlan
> rendering, and only asks the recipe author for version + URLs +
> optional sha256. Reach for the full producer contract below when
> the recipe needs dynamic control over `nfpm.yaml` itself or wants
> to emit multi-package plans from a single program.

Any program emitting a valid JSON document on stdout is a producer
that can pipe into `cooper build`. `cooper discover` is just the
built-in producer for the GitHub case.

```sh
my-producer            | cooper build -                  # always-build
my-producer | filter   | cooper build -                  # dedup against repo
cooper discover c.yaml | mutate-plan | cooper build -    # compose with built-in
```

Producers MUST emit a document whose `build_inputs_hash` recomputes
identically on the build side; see *`pkg/plan` public API* below for
the helpers cooper exposes for language-agnostic conformance.

Producers MAY emit any number of `packages[]`/`artifacts[]` entries,
mark any package as `result: "error"`, set `tool.name` to their own.
Producers MUST set `tool.format_revision` to the value of a real
cooper release they are conformant with; build re-checks the hash
against that revision's algorithm and rejects fabricated values.

### `pkg/plan` public API

`pkg/plan` is cooper's only externally-importable package. Exported
surface, in addition to the JSON schema types from *Discover JSON
contract*:

| Symbol                              | Purpose                                                                                       |
| ----------------------------------- | --------------------------------------------------------------------------------------------- |
| `ComputeBuildInputsHash(BuildPlan)` | Per-artifact hash — JCS canonicalization (RFC 8785) + SHA256                                  |
| `DeriveEpoch(input []byte) int64`   | Stable `source_date_epoch` derived from canonical input bytes; SHA-256 projected into a fixed 2020–2030 window. Used by every source kind without a canonical upstream `published_at` (json_url, xml_url, external, staves, chandler). |
| `CompareVersions(a, b string) int`  | Debian Policy §5.6.12 version comparison; returns `-1`, `0`, `+1`                             |
| `SplitDebianRevision(v string)`     | Split a Debian version: `"1.0.0-2"` → `("1.0.0", "2")`; bare `"1.0.0"` → `("1.0.0", "")`     |
| `SchemaVersion`, `FormatRevision`   | The two version knobs (see *Discover JSON contract* field notes)                              |

Conformance corpus: `pkg/plan/hash_test.go`
(`TestComputeBuildInputsHash_Golden`) for the hash;
`CompareVersions` has its own pinned corpus from Policy §5.6.12's
worked examples plus historically-tripping edge cases (tildes,
all-numeric vs. all-alpha runs, empty debian-revision, missing
epoch).

## CLI

```
cooper discover <CONFIG>     [--package NAME...] [-o OUTPUT]                  # YAML → JSON plan
cooper build    <JSON_FILE>  [--out-dir DIR] [--work-dir DIR] [--revision N]  # JSON plan → .debs + annotated JSON
                             [--stage-only | --keep-work]
cooper validate <CONFIG>                                                       # parse + lint; no network
```

Positional arguments are required (use `-` for stdin). No implicit
default config path.

- **`discover <CONFIG>`** — reads the top-level cooper config and every
  per-package multi-doc file it lists. `-o OUTPUT` writes to a file
  (`-` for stdout, the default). `--package NAME` (repeatable) narrows
  to a subset.
- **`build <JSON_FILE>`** — reads the JSON plan. `--out-dir` is where
  `.deb`s land (default `./dist/`). `--work-dir` is the staging root
  (default `./.cooper-work/`). `--revision N` (positive integer) sets
  nfpm's `release: N` field at on-disk write time so the published
  `.deb` is versioned `<bare>-N`; the `build_inputs_hash` is **not**
  recomputed (revision is post-discover metadata, not a build input —
  see *Orchestrator dedup & version policy*). Errors out when a
  recipe already claims the debian-revision slot (either `-` in
  `nfpm.version` or a non-empty `nfpm.release`). `--stage-only`: do
  everything *except* the final `packager.Package` library call;
  print one line per artifact giving the path to the resolved
  `nfpm.yaml`; leave the work dir intact. `--keep-work`: do a normal build but skip cleanup.
  `--stage-only` and `--keep-work` are mutually exclusive;
  `--revision` is compatible with either.
- **`validate <CONFIG>`** — lint without network. Checks: top-level
  config shape; every multi-doc file's two-document structure; doc 1
  schema; doc 2 well-formed YAML; in-scope path references resolve
  (literal file or `.tmpl` exists, glob has matches, directory exists,
  no traversal escape); no `<P>` and `<P>.tmpl` collisions; every
  in-scope `.tmpl` parses *and renders* cleanly against placeholder
  values from the closed variable set (rendering is required because
  `text/template` only catches unknown-field and type-mismatch errors
  at execution time); every `${KEY}` substitution in doc 2 resolves
  against the union of built-ins (`VERSION`, `ARCH`, `ARCH_GNU`,
  `ASSETS`, `SOURCE`) and per-arch `vars`, with key-grammar
  (`[A-Z][A-Z0-9_]*`) and reserved-name checks on `arches[].vars`
  entries. Per-package: a failed package is reported but doesn't
  abort the rest.

No daemon mode. Run from cron, systemd timer, or CI.

## Implementation stack

Pure Go, no CGO.

- **`github.com/goreleaser/nfpm/v2`** — imported as a library. Cooper
  writes the staged `nfpm.yaml` to disk (so `--stage-only` /
  `--keep-work` debugging is unchanged) and then calls
  `nfpm.ParseWithEnvMapping` on it with a cooper-controlled env
  mapper, materializes the `nfpm.Info`, and writes the .deb via
  `packager.Package(info, out)`. The nfpm version is pinned by
  `go.mod`; deployment containers no longer need the `nfpm` CLI on
  PATH. The user-facing contract is unchanged: the doc-2 stays
  vanilla nfpm, and the on-disk `nfpm.yaml` matches what nfpm reads.
- **`github.com/google/go-github/v86`** — releases API.
- **`gopkg.in/yaml.v3`** — multi-document YAML decode/encode.
- **`github.com/bmatcuk/doublestar/v4`** — glob expansion at discover
  time (when cooper decides which files to inline into `aux_files`).
  nfpm itself uses `goreleaser/fileglob` (gobwas/glob underneath); for
  the simple `./completions/*`-style globs that cooper supports, the
  match sets agree. If a divergence ever surfaces, switch cooper's
  expansion to fileglob — the user-visible doc 2 is unchanged.
- **`archive/tar`**, **`compress/gzip`**, **`github.com/ulikunitz/xz`**,
  **`github.com/klauspost/compress/zstd`**, **`archive/zip`** — archive
  readers.

Cooper is **stateless**: no state files, no cross-run memory.

## Package layout

Cooper shares the apt-wharf Go module with the other tools. Its
internal packages are namespaced under `internal/cooper/`; signpost's
sit alongside under `internal/signpost/`; cross-cutting helpers
shared by both (secret loading, process-group kill, the go-github
wrapper) live flat under `internal/`.

```
cmd/cooper/                 main, subcommand dispatch (validate, discover, build)
pkg/plan/                   public: JSON contract types, JCS, build_inputs_hash
                            (importable by external producers)
internal/cooper/config/     cooper.yaml + multi-doc package file parsing/validation
internal/cooper/source/     GitHub release + asset resolution
internal/cooper/version/    version-string assembly per version_from + version_template
internal/cooper/stage/      aux file walker, template renderer, doc 2 substitution,
                            aux_files materialization (used by both discover and build)
internal/cooper/discover/   per-package orchestrator emitting plan.Plan
internal/cooper/build/      download, sandboxed archive extract, staging,
                            mtime pin, in-process nfpm Package, .deb hashing
```

## Security & sandboxing

Two trust boundaries: the upstream asset (GitHub-hosted; vendor
compromise can't be ruled out) and the package multi-doc file's
directory (cooper inlines from there into `aux_files`).

For asset extraction:

- Hard caps on uncompressed size (default 1 GiB) and file count
  (default 100 000) per asset.
- Recipes that legitimately need higher caps (large IDEs, framework
  distributions) declare them explicitly in the sidecar's `extract:`
  block — `max_bytes` accepts humanized suffixes (`8GiB`, `4096MB`,
  bare byte counts), `max_files` is a positive int. Both knobs fall
  back to the defaults above when unset. A hard ceiling of 64 GiB /
  2 000 000 files applies on top, so a malicious or compromised
  recipe can't disable the safety net entirely.
- Refuse `..` path components, absolute paths, `\0`.
- Refuse symlinks/hardlinks pointing outside the destination prefix.
- Refuse devices, FIFOs, sockets, setuid/setgid bits. Modes masked
  to `0o7777`.
- Asset SHA256 verified against the JSON before any bytes are
  unpacked. If `asset.sha256` is `null`, build streams + hashes
  during download and records the value in the re-emitted JSON.

For the package directory:

- Every literal-file path, glob match, and recursively-staged
  directory entry is resolved with `..` and symlink resolution; the
  final path must remain inside the package multi-doc file's
  directory. Escapes are hard discover errors.
- Symlinks pointing outside the package dir are rejected even when
  readable (target's resolved path matters, not whether reading
  succeeds).
- Refuse `\0` in any aux-file path; reject path components longer
  than 255 bytes.

## Reproducibility

Cooper guarantees **byte-identical `.deb` output** for a given JSON
plan. Two `cooper build` invocations on the same plan, on any host,
at any time, produce two `.deb`s with the same SHA256. This is a
hard property — it's what lets `build_inputs_hash` serve as the
orchestrator's primary dedup key (see *Orchestrator dedup & version
policy*).

How it's pinned:

- `SOURCE_DATE_EPOCH` from `build_plan.source_date_epoch` is exported
  into nfpm's env so all `ar` and inner-tar mtimes are fixed. For
  the built-in `github_release` discoverer, the epoch is the
  release's `published_at` parsed as RFC 3339 and converted to
  integer Unix seconds (no rounding; field is already second-resolution
  in the GitHub API). External producers set their own value;
  whatever they choose, it must be deterministic given their inputs.
  Cooper rewrites the mtime of every staged file to this value
  before invoking nfpm.
- nfpm version is named in cooper's release notes;
  `tool.format_revision` bumps whenever cooper's output `.deb` bytes
  would diverge for the same logical inputs (see *Discover JSON
  contract*).
- nfpm's environment is scrubbed to only `VERSION`, `ARCH`,
  `ASSETS`, `SOURCE_DATE_EPOCH`, `LC_ALL=C`, and a minimal fixed
  `PATH`. Cwd is the staging dir. Hostname, user, and inherited
  env cannot reach the build.
- File ownership is `0/0` on every entry. Cooper rejects any nfpm
  document setting non-zero `owner`/`group` on a `contents:` entry.
- Tar entries are sorted by destination path (nfpm's default);
  cooper does not rely on `readdir` order anywhere.
- No wall-clock reads in the build path. Only timestamp source is
  `source_date_epoch`.

A CI self-check builds a fixed corpus of JSON plans twice and diffs
SHA256s. Any drift fails the build, catching nfpm-upgrade or
pipeline regressions before release. The current implementation of
this check lives at `internal/cooper/build/run_e2e_test.go`
(`TestRun_RealNfpm`); it skips automatically when nfpm isn't on PATH
so the rest of the test suite stays portable.

## Implementation status

v0 is complete as of the implementation pass that landed
`f17d3f6 add cooper build`. All three subcommands work end-to-end:

- `cooper validate <CONFIG>` — network-free lint of cooper.yaml + every
  per-package multi-doc file: doc 1 schema, doc 2 well-formedness,
  aux-file path resolution, template parse + render against placeholder
  Vars.
- `cooper discover <CONFIG>` — emits a `plan.Plan` JSON document. One
  GitHub API call per package (the cached release powers per-arch asset
  matching). Token resolution via cooper.yaml's `github.token_env` /
  `token_file` (mutually exclusive); unauthenticated when neither is
  set.
- `cooper build <JSON_FILE>` — produces reproducible `.deb` files.
  Recomputes `build_inputs_hash` on every artifact before any I/O;
  rejects mismatches. Per-artifact failure isolation. Real-nfpm e2e
  test asserts byte-identical `.deb` output across two builds of the
  same plan.

Test coverage at v0 release:
- `pkg/plan` — round-trip + JCS sort + golden hash vector + sensitivity
  (every input flips the hash, excluded fields don't).
- `internal/cooper/config` — 30+ validation cases.
- `internal/cooper/stage` — every aux-resolution path-shape case from
  the design plus traversal/symlink-escape rejection.
- `internal/cooper/source` — httptest-mocked GitHub for all four
  release modes plus prerelease opt-in.
- `internal/cooper/version` — every version_from variant, plus
  version_template substitution and Debian-grammar rejection.
- `internal/cooper/build` — every archive-extraction security
  rejection (traversal, absolute, setuid, device, symlink-out,
  hardlink-out, byte cap, file cap), plus end-to-end orchestrator
  against the real in-process nfpm library, including a
  reproducibility check and the short-version (`4.32`) padding
  regression covered by `TestRun_RealNfpm_ShortVersionNoPadding`.

## Source kinds

Cooper ships four built-in discovery backends. The recipe's `source:`
block picks one (validation enforces exactly-one):

- **`github_release`** (the original): fetch a GitHub release, match
  per-arch assets by exact name with `${VERSION}` substitution.
  `source_date_epoch` comes from the release's `published_at`.
  Optionally set `source.github.source_archive: true` to also fetch
  the auto-generated source archive
  (`https://github.com/<repo>/archive/refs/tags/<tag>.tar.gz`)
  alongside the named binary assets. The source archive is staged
  under `${SOURCE}/` instead of `${ASSETS}/` so recipes can pull
  contrib scripts, completions, or other files from upstream
  source without losing the binary-release path. Three valid
  shapes: binary-only (source_archive omitted; `arches[].asset`
  required, today's default), archive-only (source_archive: true,
  no `arches[].asset` set — use `arches: {all: {}}`), or both.
- **`json_url`**: GET a vendor JSON endpoint, extract the version via
  a gjson path, render per-arch URLs from `arches[].asset_url`
  (singular) or `arches[].asset_urls` (plural; same multi-asset
  shape as github_release). Templates support `${VERSION}` /
  `${ARCH}` plus signpost-style `{token}` / `{gjson.path}`
  placeholders. `source_date_epoch` is derived deterministically
  from `(url, version)` via `plan.DeriveEpoch` (cooper's shared
  helper for sources without a canonical "published_at"
  equivalent — projects a SHA-256 of canonical input bytes into a
  fixed 2020–2030 window).
- **`xml_url`**: XML counterpart to `json_url`. Targets vendors who
  publish version metadata as XML — JetBrains' `updates.xml`
  (`https://www.jetbrains.com/updates/updates.xml`) is the canonical
  case, and RSS / Atom release feeds (Apache projects, some Mozilla
  downloads) fit the same shape. Schema:
  `source.xml_url: { url, version_xpath, version_strip_prefix }`;
  `arches[].asset_url` (singular) or `arches[].asset_urls` (plural)
  supports `${VERSION}` / `${ARCH}` and `{xpath:<expr>}` placeholders
  against the same body, mirroring json_url's `{gjson.path}` slot.
  The `xpath:` prefix is required — raw XPath grammar (`[`, `]`,
  `/`, `=`) inside braces would clash with the surrounding URL
  grammar. `source_date_epoch` is derived from `(url, version)` via
  `plan.DeriveEpoch`, the same shared helper json_url uses. XML
  parsing uses `github.com/antchfx/xmlquery`.
- **`external`**: exec a user-supplied script and read `{version,
  assets[]}` JSON from its stdout. The script owns the upstream-
  specific scraping; cooper continues to own everything downstream
  (`build_inputs_hash`, `source_date_epoch`, aux_files, BuildPlan
  rendering). Schema: `source.external: { command, env, env_forward, timeout }`.
  Use this for vendors that don't fit github / json / xml — Apache
  project autoindexes (apache directory-studio is the canonical
  case), vendor "click here to download" wrappers, SourceForge,
  Maven artifacts, ad-hoc HTML. See *External source script
  contract* below.

For sources still outside this set — typically because the recipe
needs dynamic control over the `nfpm.yaml` itself, not just version
plus URL — fall back to the heavier *external producer* pattern:
emit a full `plan.Plan` JSON document from any program and pipe it
into `cooper build -`. External source covers most of the long tail
that previously required producers; the producer pattern is now the
escape hatch for cases where cooper's BuildPlan rendering itself is
insufficient.

### External source script contract

The recipe field shape:

```yaml
source:
  external:
    command: ["./discover.sh"]   # path resolved relative to cooper.yaml's dir
    env:                          # optional; literal key/value pairs
      FOO: bar
    env_forward:                  # optional; forward from caller's env if set
      - GITHUB_TOKEN
    timeout: 30s                  # optional; default 30s (time.ParseDuration)
```

Cooper exec's `command` with `cwd` set to the cooper.yaml's directory
(so `./discover.sh` resolves naturally regardless of where cooper was
invoked from) and a stripped environment built in three layers:

1. **Proxy allowlist** — `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` plus
   lowercase forms, forwarded from the caller's environment if set.
2. **`env_forward`** — caller-named vars (typically `GITHUB_TOKEN` and
   similar secret/auth tokens) forwarded from the caller's environment.
   Names not set on the caller are silently dropped, matching the proxy-
   allowlist behavior — recipes don't have to predict the deployment
   environment exactly. Validation rejects empty names and names
   containing `=` / NUL.
3. **`env`** — literal key/value pairs from the recipe.

Later layers win on key collision (standard `exec.Cmd` behavior), so
the recipe's literal `env:` always overrides whatever the caller's
environment happened to carry. The child-process discipline matches
signpost's `internal/signpost/source/external.go`: proc-group SIGKILL on
timeout, stderr captured and surfaced in error messages, exit 0 =
success.

The script reads no stdin and writes one JSON document to stdout:

```json
{
  "version": "2.0.0.v20210717-M17",
  "assets": [
    {
      "arch":   "amd64",
      "url":    "https://dlcdn.apache.org/.../linux.gtk.x86_64.tar.gz",
      "sha256": "<hex>"
    }
  ]
}
```

`assets[].arch` must match a key in the recipe's `arches:` map;
cooper rejects an asset whose arch isn't declared. `assets[].sha256`
is optional (see *Trust model* below). Single-arch sources emit one
`assets[]` entry; multi-arch sources emit N.

**Version.** When `source.external:` is set, the version comes from
the script's `.version` field. `version_from`, `version_template`,
and `version_regex` are rejected at validate time — the script names
the version directly; layering a separate extraction step on top
would just add ambiguity. `epoch` and the Debian-revision knobs
(`--revision`, auto-bump) still apply at the recipe layer.

**Asset URL validation.** The script's `url` is authoritative.
`arches[].asset_url` is optional for external sources: if absent,
cooper trusts the script. If present, it's treated as a validation
template — cooper renders it (with `${VERSION}` / `${ARCH}`
substitutions) and rejects the discover if the script's URL doesn't
match. Opt-in URL-drift guardrail; useful when the recipe author
wants to pin the URL shape independent of the script's scraping
logic.

**Trust model: SHA256.** If the script returns `sha256`, cooper
trusts it and writes it into `plan.Asset.SHA256` directly from
discover — Apache `.sha256` sidecars and other vendor-published
digests are exactly the upstream authority for that bytestream. If
`sha256` is absent, cooper falls back to "compute at build time"
(matching json_url's current behavior). Cooper does **not** fetch
the asset at discover time to verify the script's hash; that would
blur the discover-vs-build network split. The script is in the
recipe's TCB — if you don't trust your own discovery script you
have a different problem.

**source_date_epoch.** Derived deterministically from
`(command path, version)` via `plan.DeriveEpoch`, the same shared
helper json_url and xml_url use. The script does not supply an
epoch; letting it would let two consecutive discover runs of the
same upstream produce different epochs and thus different
`build_inputs_hash` values.

**Validate.** `cooper validate` checks that `command[0]` exists and
is executable when its path starts with `./` or `/`; bare names are
assumed to resolve on `PATH` (cooper does not try to model the
runtime environment). Validate never exec's the script — lint stays
fully offline.

### HTML-scraped sources

We explicitly **do not** ship an `html_url` source kind, and don't
plan to. Two reasons drive that:

1. **Fragility is asymmetric.** A json_url or xml_url recipe breaks
   when the vendor renames a field; that's rare and easy to diagnose.
   An html_url recipe breaks every time the vendor restyles their
   download page, sometimes silently (a selector still matches but
   resolves to the wrong element). Every recipe becomes a maintenance
   liability the way json_url / xml_url recipes don't.
2. **Most "needs scraping" cases have a hidden API.** Flutter exposes
   `storage.googleapis.com/flutter_infra_release/releases/releases_linux.json`;
   Android Studio has `developer.android.com/studio/archive`; most
   "JS-rendered download page" vendors load their actual data from a
   discoverable JSON endpoint. Reach for the vendor's network tab
   before reaching for a CSS selector.

The residual long-tail of genuinely HTML-only sources is best handled
as an `external` source (see *External source script contract*). A
typical scraper script is ~20 lines of shell:

```sh
#!/bin/sh
# Emits {version, assets[]} to stdout. Cooper handles everything else.
set -eu

version=$(
  curl -fsSL https://example.com/downloads/ \
    | grep -oE 'foo-[0-9]+\.[0-9]+\.[0-9]+\.tar\.gz' \
    | head -1 \
    | sed 's/^foo-//; s/\.tar\.gz$//'
)
url="https://example.com/foo-$version.tar.gz"
sha256=$(curl -fsSL "$url.sha256" | awk '{print $1}')

jq -n --arg v "$version" --arg u "$url" --arg s "$sha256" '{
  version: $v,
  assets: [{ arch: "amd64", url: $u, sha256: $s }]
}'
```

The script owns the fragility — when the vendor restyles, the recipe
author updates their own grep/sed, not a cooper recipe. Cooper keeps
a small, declarative surface area; the long tail of one-off scrapers
lives where the maintenance reality already is.

For recipes that need to control more than version + URLs — e.g.
when the `nfpm.yaml` itself has to be computed at discover time —
fall through to the heavier external-producer pattern below.

## Deferred (post-v0)

- **Upstream-supplied SHA256 sidecars (`sha256_asset` / `sha256_path`)** — many
  vendors publish a per-asset SHA256 alongside the binary, either
  as a sidecar file (`foo.tar.gz` + `foo.tar.gz.sha256`) or as a
  field in their JSON feed. Cooper today only carries SHA256 in
  the plan when the github_release `digest` API field provides it
  or when an `external` source's script returns `assets[].sha256`;
  json_url ignores any embedded hash; sidecar files aren't fetched
  for github_release / json_url / xml_url. Two related extensions:
    - `arches[].sha256_asset:` for github_release recipes —
      naming the sidecar asset; cooper fetches its body and
      treats the (newline-trimmed, first whitespace-delimited
      token) value as the expected SHA256.
    - `source.json_url.sha256_path:` and analogous on `xml_url` —
      a structured path into the same response body that yielded
      the version; the resolved string is the asset's SHA256.

  Hash flows into `plan.Asset.SHA256` so drayman's dedup-by-hash
  query works on first contact instead of waiting for cooper to
  stream-hash on first build. Modest supply-chain win (still TOFU
  on the URL itself, but at least we've pinned what bytes that URL
  is supposed to deliver). Pairs naturally with a future GPG /
  sigstore verification step.
- **Shared HTML-autoindex helper** (`cmd/discover-apache-autoindex/` or
  similar) — several packages migrating off the legacy reprepro tree
  (Apache directory-studio, ZooKeeper, etc.) will likely all want to
  scrape an Apache-style autoindex for the latest version subdir plus
  a tarball + sidecar `.sha256`. Build a few per-package
  `discover.sh` scripts first; factor the common shape into a
  helper binary (or a shell library shipped alongside) only once the
  boilerplate is concrete. Don't gate the external-source v1 on
  having this helper ready.
- **Glob in `cooper.yaml`'s `packages:`** — ergonomics; explicit list is fine for v0.
- **`--prefetch-hashes` for missing `asset.sha256`** — current contract (build streams + hashes; orchestrator re-imports unconditionally) loses dedup for that artifact. Wait for a real package that surfaces it.
- **`version_template` under json_url** — for vendors whose extracted version needs post-processing beyond `version_strip_prefix` (e.g. regex-based extraction, composing multiple gjson fields into one version). Out of scope for v0; users can re-tag in their JSON or use an external producer.
- **`.Vars` / `.ArchGNU` in aux templates** — `arches[].vars` and
  `${ARCH_GNU}` are doc-2 substitutions only. Aux `.tmpl` files
  (systemd units, postinstall scripts, etc.) still see just the
  closed `.Name` / `.Version` / `.Arch` / `.Epoch` / `.PublishedAt`
  set. Symmetry would let a unit file reference `{{ .Vars.TARBALL_DIR }}`
  for the same per-arch literal doc 2 uses; revisit when a template
  hits the same pain. Each addition is a one-way door under
  `format_revision`.
