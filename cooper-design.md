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
assets, stages files at the paths doc 2 references, and exec's `nfpm pkg`
per artifact to emit `.deb`s. The JSON between phases is the
orchestration boundary **and** the plugin point: any program emitting
valid JSON is a producer (see *External producers*).

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
artifact's on-disk `nfpm.yaml` — the third on-disk-only adjustment
alongside `expand: true` and the `X-Cooper-Build-Inputs-Hash`
injection. nfpm's deb packager concatenates `<version>-<release>`
into the resulting Debian Version, so the published `.deb`'s version
is `<bare>-N` and its filename is `<name>_<bare>-<N>_<arch>.deb`.
**The `build_inputs_hash` is unchanged.** The revision is
post-discover metadata, not a build input; including it would mean
re-discovery never hits the dedup branch (infinite re-bumping).

Cooper uses nfpm's dedicated `release:` field rather than rewriting
`version:` because nfpm's default `version_schema: semver` interprets
a `-N` suffix on `version:` as a semver prerelease and emits it as
`~N` (Debian's tilde-prerelease) in the published `.deb`.

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

There are no `paths:` keys — output and work directories are CLI flags
on `cooper build` (see *CLI*), not in `cooper.yaml`. They're build-time
concerns, and discover and build can run on different hosts.

### Per-package multi-doc file

Two YAML documents in one file, separated by `---`. Doc 1 is cooper;
doc 2 is vanilla nfpm. Strict ordering — no `kind:` discriminator,
since putting one in doc 2 would break the "vanilla nfpm" property.

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

Cooper validates: exactly two documents; doc 1 has `source:`; doc 2 has
`name:`. Anything else is a hard config error.

### Doc 1 schema

```
source.github.repo            <owner>/<name>           required
source.github.release         "latest"                 default
                              { tag_pattern: REGEX }
                              { tag: STRING }
source.github.include_prerelease  bool                 default false
version_from                  tag | tag_strip_v | asset_filename | fixed
version_template              STRING                   optional
version_regex                 STRING                   required when
                                                       version_from == asset_filename
epoch                         int                      default 0
arches                        map<arch, { asset: STRING }>  ≥ 1 entry
```

Doc 1's only string-substitution surface is the `arches.<arch>.asset:`
value. The single legal substitution is `${VERSION}` (uppercase, same
syntax as doc 2; resolved per *Version selection* below). After
substitution, the result is matched for **exact equality** against the
release's asset names. Zero or more-than-one matches → discover error.

### Cooper's substitution into doc 2

Cooper substitutes a fixed set of three variables into doc 2;
anything else passes through verbatim.

| Variable     | Resolved by | Where in JSON                                    |
| ------------ | ----------- | ------------------------------------------------ |
| `${VERSION}` | discover    | substituted to a literal in `build_plan.nfpm`    |
| `${ARCH}`    | discover    | substituted to a literal in `build_plan.nfpm`    |
| `${ASSETS}`  | build       | left symbolic; build sets it as an env var when exec'ing nfpm |

For nfpm to expand `${ASSETS}` at build time, every `contents[]`
entry needs `expand: true`. Cooper sets that flag (preserving any
explicit user value) when writing the on-disk `nfpm.yaml`; see
*Build* step 4 for the full set of on-disk-only adjustments.

That's the full set. There is no `${AUX}`, no `${TMPL_*}`, no
cooper-specific env-var namespace beyond `${ASSETS}`.

### Aux file resolution (and templating)

For two — and **only two** — sections of doc 2, cooper makes sure files
referenced by relative path actually exist when nfpm runs:

1. `contents[].src`
2. `scripts.preinstall`, `scripts.postinstall`, `scripts.preremove`,
   `scripts.postremove`

Every other field — `name`, `description`, `depends`, `changelog`,
`deb.fields.*`, `deb.signature.*`, `rpm.*`, `apk.*`, anything else —
is **passthrough**. Cooper does not stage files for those fields,
and nfpm runs with cwd set to the staging dir (which contains only
files cooper materialized). A passthrough relative path like
`changelog: ./CHANGELOG.md` will not resolve unless the user uses
an absolute path; cooper doesn't touch these fields.

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
       b. Substitute ${VERSION} and ${ARCH} into doc 2 → resolved nfpm
          config (per arch).
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

Discover is read-only and side-effect-free. A failed package becomes an
`error` entry; it does not abort the run. Network use is bounded —
one GitHub releases API call per package, plus optional `HEAD` per
asset when SHA256 isn't on the release payload.

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
     build_plan.nfpm with three on-disk-only adjustments:
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
     None of these adjustments flow back into the JSON, so none of
     them perturb build_inputs_hash.
  5. exec `nfpm pkg --packager deb -f nfpm.yaml -t <out-dir>/` with:
       cwd = <staging>
       env (everything else dropped):
         VERSION=<from JSON>     ARCH=<from JSON>
         ASSETS=<staging>/asset  SOURCE_DATE_EPOCH=<from JSON>
         LC_ALL=C  PATH=<minimal, fixed; resolves only nfpm>
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
          "asset": {
            "name":   "hugo_extended_0.140.0_linux-amd64.tar.gz",
            "url":    "https://github.com/.../linux-amd64.tar.gz",
            "size":   19283746,
            "sha256": "sha256:abc123...",
            "sha256_source": "github_api"
          },
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
        { "arch": "arm64", "asset": { "...": "..." }, "deb": { "...": "..." }, "build_plan": { "...": "..." } }
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
- **`build_plan.nfpm`** is doc 2 with `${VERSION}` and `${ARCH}`
  substituted to literals; `${ASSETS}` left symbolic. Globs and
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
  references resolve naturally with cwd set to the staging dir. Raw
  inlined files and rendered templates are indistinguishable here —
  intentional.
- **`deb.path` / `deb.sha256`** are `null` in discover output and
  populated in build output. Same JSON shape on both sides.
- **Artifact-level `result` / `error`** are absent on discover output
  (artifact failures collapse into a package-level error there) and
  populated by build only when an individual artifact's pipeline
  failed. Sibling artifacts in the same package finish regardless. An
  empty `result` on an artifact means "ok" — the field uses
  `omitempty`.
- **`error.kind`** values: `discovery_failed` (GitHub API or asset
  resolution); `version_invalid` (resolved version doesn't match
  Debian's grammar); `aux_resolution_failed` (template render error,
  missing file, glob with no matches, etc.); `build_failed`
  (build-only; nfpm exec, asset SHA mismatch, archive sandbox
  rejection, or any other build-pipeline failure).
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
    "asset_sha256":    artifacts[i].asset.sha256,
    "build_plan":      artifacts[i].build_plan
}))
```

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
  everything *except* the final `nfpm pkg` exec; print one line per
  artifact giving the path to the resolved `nfpm.yaml`; leave the
  work dir intact. `--keep-work`: do a normal build but skip cleanup.
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
  at execution time). Per-package: a failed package is reported but
  doesn't abort the rest.

No daemon mode. Run from cron, systemd timer, or CI.

## Implementation stack

Pure Go, no CGO.

- **`nfpm` (CLI subprocess)** — cooper exec's `nfpm pkg` against an
  on-disk `nfpm.yaml`. Cooper does **not** import
  `github.com/goreleaser/nfpm/v2` as a library; the user-facing
  contract is "this is the nfpm.yaml nfpm sees." nfpm's expected
  version is named in cooper's release notes.
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

Cooper currently shares a Go module with apt-signpost, so its internal
packages are namespaced under `internal/cooper/` to avoid colliding
with the existing apt-signpost internals (`internal/config`,
`internal/source`, etc.).

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
                            mtime pin, exec nfpm, .deb hashing
```

## Security & sandboxing

Two trust boundaries: the upstream asset (GitHub-hosted; vendor
compromise can't be ruled out) and the package multi-doc file's
directory (cooper inlines from there into `aux_files`).

For asset extraction:

- Hard caps on uncompressed size (default 1 GiB) and file count
  (default 100 000) per asset.
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
  (stub-exec) and end-to-end real-nfpm with reproducibility check.

## Deferred (post-v0)

- **Glob in `cooper.yaml`'s `packages:`** — ergonomics; explicit list is fine for v0.
- **`--prefetch-hashes` for missing `asset.sha256`** — current contract (build streams + hashes; orchestrator re-imports unconditionally) loses dedup for that artifact. Wait for a real package that surfaces it.
- **Second built-in source kind: `json_url`** — discovers a release by GETting a vendor JSON endpoint, extracting the version via a path expression (gjson syntax, mirroring apt-signpost's existing `json_url` discoverer), and synthesizing a release/asset tuple with a templated download URL. Targets the not-rare-enough vendor pattern of "publish releases off-GitHub on a stable JSON feed" (zoom, go.dev, claude downloads, etc.). Schema sketch: `source.json_url: { url, version_path, asset_url_template, sha256_path? }`. Promotes the `internal/source/json_url.go` primitive from apt-signpost into cooper's discover surface; the rest of the pipeline (version assembly, doc 2 substitution, build_inputs_hash) reuses the existing code paths. Out of scope for v0 because GitHub covers the bulk of the realistic backlog and this expansion shouldn't gate the v0 release.
