# apt-chandler — Design

## Problem

Third-party apt repos are typically bootstrapped by following a vendor
README that reads, almost verbatim:

```sh
curl -fsSL https://download.docker.com/linux/debian/gpg \
    | sudo gpg --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg
echo "deb [signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] \
    https://download.docker.com/linux/debian bookworm stable" \
    | sudo tee /etc/apt/sources.list.d/docker.list
```

That ritual is fine the first time. It rots from there:

- Whoever installed it has no record of *what* they installed; the
  next operator (or the next box) re-runs the same shell soup.
- The key on disk is opaque — when the vendor rotates, nobody knows
  to refresh it.
- The `.list` file is legacy one-line format, lacking the
  per-stanza `Enabled:` toggle and other deb822 affordances.
- Multiple sources from the same vendor (stable + experimental, or
  the same repo across several distros) duplicate the shell snippets.

apt-chandler turns the same vendor instructions into a YAML config
that produces a reproducible `.deb`. The `.deb` ships the dearmored
keyring under `/usr/share/keyrings/`, a deb822 `.sources` file (one
per source) under `/etc/apt/sources.list.d/`, and gets installed,
upgraded, and removed like any other package.

The trade-theme name is direct: a *chandler* historically supplied
ships and ports with provisions — candles, ropes, salt pork, the
trust material a voyage required. apt-chandler supplies systems
with the trust material (signing keys) and routing information
(`.sources` files) they need to reach third-party apt repos.

## Approach

Same two-phase JSON-contract shape as [apt-cooper](./cooper-design.md)
and apt-staves: chandler owns only the discovery side; cooper-build
does the actual `.deb` packaging.

```
       YAML config                                       JSON plan                  .deb files
            │                                                │                          │
            ▼                                                ▼                          ▼
      ┌──────────┐  HTTPS key fetch + dearmor          ┌──────────┐                ┌──────────┐
      │ discover │ ──────────────────────────────────► │ JSON plan│ ─────────────► │  build   │
      └──────────┘  deb822 stanza rendering            └──────────┘  (cooper)      └──────────┘
                                                              │
                                                              ▼
                                                        orchestrator (drayman):
                                                        dedup by build_inputs_hash,
                                                        auto-bump revisions
```

`chandler discover <CONFIG>` reads a chandler YAML config, fetches
GPG key bytes from the URLs the config names, dearmors them in-process,
renders one `.sources` stanza per source, and emits a `plan.Plan` JSON
document on stdout. `cooper build -` consumes that plan and emits
`.deb` files exactly as it would for cooper's own GitHub-release
recipes.

`chandler discover c.yaml | cooper build -` is the no-orchestrator
case. Dedup and revision auto-bump live in the orchestrator (drayman),
same as cooper.

## Output & downstream publishing

Chandler emits no bytes of its own — every `.deb` is produced by
cooper-build from the plan chandler hands it. The contract chandler
honors with cooper-build:

- One `plan.Package` per matrix entry (one package per target in
  matrix mode; one package total in simple mode).
- Source kind `plan.SourceKindChandler = "chandler"`.
- `Asset.URL == ""` on every artifact, so cooper-build's download
  step is a no-op (same affordance staves relies on).
- All file bytes shipped inline as `build_plan.aux_files` entries,
  base64-encoded — including the dearmored keyring and the rendered
  `.sources` files.
- `source_date_epoch` derived deterministically from the resolved
  per-target content (canonical nfpm subtree, every aux file, and
  the matrix distro/codename) via `plan.DeriveEpoch`. Same recipe
  + same fetched keys + same target → same epoch on every host.
  Git is not required; `git_commit` / `git_date` on `plan.Source`
  are best-effort provenance only.
- Per-artifact `build_inputs_hash` computed by `pkg/plan`'s
  `ComputeBuildInputsHash` exactly as cooper would.

That last point matters: chandler's reproducibility, dedup semantics,
and drayman handoff are all inherited from cooper-build for free.
Chandler doesn't get to be reproducible by trying — it gets to be
reproducible by emitting a valid plan.

## Trust model

HTTPS to the vendor's download host is the only trust anchor.
Chandler does not verify GPG fingerprints, signatures, or checksum
sidecars. The framing: if a user is willing to follow `curl
https://vendor.example/key | sudo tee ...` from a README, they're
trusting HTTPS + the CA system. Chandler inherits that trust model
rather than demanding a fingerprint the user would have no idea how
to source (other than from the same vendor website that gave them
the URL).

What chandler does for the user:

- Logs every fetched key's UID, fingerprint, and expiry to stderr at
  discover time, so a human eyeballing a new package can sanity-check.
- Inlines the dearmored key bytes into the plan, so once a package
  is built, the trust material is pinned in `build_inputs_hash`. Any
  vendor-side key rotation produces a different hash → a new
  revision → an upgrade users actually receive through apt.

## Config

### Top-level shape

```yaml
package:
  name: docker-archive-keyring
  version: "1.0"
  description: APT sources and signing key for Docker CE.
  maintainer: Ophymx <ops@ophymx.com>
  homepage: https://docs.docker.com/engine/install/debian/   # optional
  depends:  []                # optional; merges with auto-injected apt (>= 1.5)
  conflicts: []               # optional
  replaces:  []               # optional
  provides:  []               # optional
  run_apt_update: false       # default; when true, postinst runs apt-get update

keys:
  docker-ce:
    url: https://download.docker.com/linux/debian/gpg

sources:
  - id: docker-ce
    uris: https://download.docker.com/linux/debian
    suites: bookworm
    components: stable
    architectures: [amd64, arm64]
    key: docker-ce
```

That's the simple case: one package, one key, one source. Discover
emits exactly one `plan.Package` with one `Artifact` (arch `all`).
The resulting `.deb` installs:

```
/usr/share/keyrings/docker-ce.gpg                       # binary, dearmored
/etc/apt/sources.list.d/docker-ce.sources               # conffile
```

### Schema

```
package.name                  string                   required; templated if matrix>1
package.version               string                   required; bare base (drayman bumps)
package.description           string                   required
package.maintainer            string                   required
package.homepage              string                   optional
package.depends               list<string>             optional; merges with apt (>= 1.5)
package.conflicts             list<string>             optional
package.replaces              list<string>             optional
package.provides              list<string>             optional
package.run_apt_update        bool                     default false

keys                          map<slug, KeyEntry>      ≥ 1 entry
keys.<slug>.url               string                   exactly one of {url, urls}; HTTPS fetched + dearmored
keys.<slug>.urls              list<string>             exactly one of {url, urls}; fetched, dearmored, concatenated

targets                       list<Target>             optional; absent → simple mode
targets[].distro              string                   required (debian, ubuntu, ...)
targets[].codename            string                   required (bookworm, trixie, noble, jammy, ...)

sources                       list<Source>             ≥ 1 entry
sources[].id                  string                   required; [a-z0-9-]+, unique within YAML
sources[].types               string | list<string>    default "deb"
sources[].uris                string | list<string>    required
sources[].suites              string | list<string>    required
sources[].components          string | list<string>    required
sources[].architectures       string | list<string>    optional; omitted → apt default
sources[].key                 string                   required; references a slug in keys{}
```

Unknown YAML keys are a hard parse error. There is no `raw:`
passthrough into deb822 or nfpm; new fields drive new typed schema
entries instead (see [[feedback-schema-strict-no-passthrough]]).

### Templating (matrix mode)

When the YAML declares a top-level `targets:` list with > 1 entry,
chandler runs in matrix mode: discover fans out one `plan.Package`
per target, with template tokens in source fields and the package
name expanded per-target.

```yaml
package:
  name: "postgresql-archive-keyring-{{.Codename}}"      # template required in matrix
  version: "1.0"
  description: APT sources and signing key for PostgreSQL.
  maintainer: Ophymx <ops@ophymx.com>

targets:
  - { distro: debian, codename: bookworm }
  - { distro: debian, codename: trixie }
  - { distro: ubuntu, codename: noble }
  - { distro: ubuntu, codename: jammy }

keys:
  pgdg:
    url: https://www.postgresql.org/media/keys/ACCC4CF8.asc

sources:
  - id: pgdg
    uris: https://apt.postgresql.org/pub/repos/apt
    suites: "{{.Codename}}-pgdg"
    components: main
    key: pgdg
```

Discover emits four `plan.Package` entries:
`postgresql-archive-keyring-bookworm`, `-trixie`, `-noble`, `-jammy`.
Each ships one `pgdg.sources` file with its target's `Suites:` string
substituted in. The single `pgdg.gpg` keyring is identical across all
four .debs.

Template engine is Go `text/template` with a closed variable set:

| Variable      | Type   | Value                                       |
| ------------- | ------ | ------------------------------------------- |
| `.Distro`     | string | e.g. `debian`, `ubuntu`                     |
| `.Codename`   | string | e.g. `bookworm`, `trixie`, `noble`, `jammy` |

No `funcMap`, no env access, no `now`. The set is a one-way door —
every entry becomes part of `build_inputs_hash` once shipped.

Templating applies in these fields only:

- `package.name`
- `sources[].uris`, `.suites`, `.components`, `.architectures`,
  `.types`
- `keys.<slug>.url`, `.urls` (rare; mostly key URLs don't need per-codename
  variation, but supported for vendors who ship per-codename keys)

Keys, slugs, source IDs, and structural fields (`enabled`, etc.) are
literal — never templated.

Validator rules specific to matrix mode:

- `targets` defined but no field varies across them → error
  ("3 targets but resolved Plans are identical — drop the matrix
  or vary at least one field").
- Template token (`{{...}}`) anywhere but `targets:` absent → error
  ("template used without targets defined").
- `targets` has > 1 entry but `package.name` lacks a template token
  → error ("matrix has 2 targets; package.name must template to
  produce distinct .deb names").

### Multi-source layout

Each source produces one file at
`/etc/apt/sources.list.d/<source-id>.sources`. The file is declared
a dpkg conffile so operator edits survive package upgrades
(critical for `Enabled: yes` → `no` toggle persistence).

Source IDs must be unique within the YAML (they're filenames). They
should be vendor-prefixed by convention (`pgdg`, `docker-ce-stable`,
`tailscale-stable`); dpkg's cross-package file-conflict detection is
the safety net if two installed keyring packages claim the same
filename.

The keyring `.gpg` file at `/usr/share/keyrings/<slug>.gpg` is **not**
a conffile — vendor key rotation should overwrite cleanly on upgrade,
no dpkg prompt, no operator hand-editing.

### deb822 stanza rendering

Per-source stanza format. `Enabled: yes` is always rendered (operator
flips to `no` with a one-character edit; conffile machinery preserves
the change across upgrades). No comment headers above the stanza —
bare deb822.

```
Enabled: yes
Types: deb
URIs: https://apt.postgresql.org/pub/repos/apt
Suites: bookworm-pgdg
Components: main
Architectures: amd64 arm64
Signed-By: /usr/share/keyrings/pgdg.gpg
```

Multi-value fields (URIs, Suites, Components, Architectures) accept
either a single string or a YAML list; render as space-separated per
deb822. `Architectures:` is omitted when the YAML doesn't specify it
(apt falls back to dpkg's default arch).

Strict v1 field set:

| YAML                  | deb822          | Default when omitted     |
| --------------------- | --------------- | ------------------------ |
| `types`               | `Types:`        | `deb`                    |
| `uris`                | `URIs:`         | required                 |
| `suites`              | `Suites:`       | required                 |
| `components`          | `Components:`   | required                 |
| `architectures`       | `Architectures:`| omitted                  |
| `key: <slug>`         | `Signed-By:`    | required; renders path   |

`Enabled: yes` is rendered unconditionally and has no YAML field —
operator toggles live in the on-disk conffile, not the source YAML.

Advanced deb822 fields (`By-Hash`, `Allow-Insecure`, `Languages`,
`Trusted`, `Valid-Until-Min`, etc.) are deliberately unsupported.
Real third-party-vendor keyring packages don't need them; if one
ever does, that's a typed schema addition, not a `raw:` escape
hatch.

### Package metadata derived without user input

Hardcoded in every chandler-emitted plan:

| Field                | Value           | Rationale                                      |
| -------------------- | --------------- | ---------------------------------------------- |
| `Architecture:`      | `all`           | No binaries in a keyring package, ever         |
| `Section:`           | `misc`          | Standard for keyring packages                  |
| `Priority:`          | `optional`      | Standard for third-party packages              |
| `Depends:`           | `apt (>= 1.5)`  | Auto-injected; user `depends:` merges in       |

The `apt (>= 1.5)` lower bound matches the apt version where deb822
`.sources` files and `Signed-By:` first stabilized. Effectively a
no-op on any current Debian/Ubuntu, but documents the requirement.

### `run_apt_update` postinst

When `package.run_apt_update: true`, chandler emits a best-effort
`apt-get update` postinst — failure is a stderr warning, not a fatal
error, since a transient vendor outage at install time shouldn't
brick the keyring package install. Rendered into `aux_files` and
referenced by `nfpm.scripts.postinstall`. Default off.

## Phases

### Discover

```
load chandler.yaml.
validate schema (no network).
git-resolve source_date_epoch:
    git log -1 --format=%ct -- <config-path>
    not-in-git → hard error.
expand targets:
    if targets[] absent: targets = [empty target] (simple mode)
    else: targets = targets[]
for each target in targets:
    1. template-substitute every templated field with this target's vars.
    2. validate the resolved package name is unique across already-seen targets.
    3. for each key slug referenced by any source[] (sorted):
         a. HTTPS GET each URL → dearmor (in-process) → concatenate.
         b. log UID / fingerprint / expiry to stderr.
         c. record dearmored bytes in this target's aux_files at "./<slug>.gpg".
         d. record key provenance (slug, source URL, fingerprint, UID, expiry) for source block.
    4. for each source in sources[]:
         a. template-substitute uris, suites, components, architectures, types.
         b. render the deb822 stanza into aux_files at "./<source-id>.sources".
         c. record source provenance.
    5. if run_apt_update: render postinst script into aux_files at "./postinst.sh".
    6. build the nfpm subtree:
         - name = resolved package.name
         - version = package.version (bare; drayman bumps revision)
         - arch = "all"
         - depends = ["apt (>= 1.5)"] ∪ package.depends
         - contents = one entry per (keyring file → /usr/share/keyrings/<slug>.gpg)
                    + one entry per (source file → /etc/apt/sources.list.d/<id>.sources,
                                                   type: config)
         - scripts.postinstall = "./postinst.sh" iff run_apt_update
    7. compute build_inputs_hash for the resolved BuildPlan.
    8. emit one packages[] entry with one artifacts[] entry (arch "all").
emit one JSON document on stdout.
```

Discover is read-only with respect to disk (no `.deb` bytes, no
staging) but does perform HTTPS key fetches. Network failure for any
key fetch → that target's package becomes a `result: "error"` entry
with `error.kind = "key_fetch_failed"`. Sibling targets in the same
matrix run finish regardless.

### Build (inherited from cooper)

Chandler does not own the build phase. `cooper build -` reads
chandler's emitted plan exactly as it reads its own. The relevant
inherited behavior, summarized from [cooper-design.md](./cooper-design.md#build):

- `Asset.URL == ""` skips download/extract for every artifact.
- `aux_files` entries materialize at `<staging>/<key>` with the
  base64 content decoded.
- `nfpm.yaml` is written from `build_plan.nfpm` with the three
  on-disk-only adjustments (`expand: true`,
  `X-Cooper-Build-Inputs-Hash` field, optional `release: N` from
  `--revision`).
- `SOURCE_DATE_EPOCH`, `mtime` pinning, scrubbed env, `LC_ALL=C`,
  and ownership `0/0` all apply unchanged.
- Reproducibility (two builds of the same plan → byte-identical
  `.deb`) is preserved.

## Discover JSON contract

Mostly inherits cooper's contract (see
[cooper-design.md § Discover JSON contract](./cooper-design.md#discover-json-contract)).
Chandler-specific deltas:

- `source.kind` is `"chandler"` (new constant
  `plan.SourceKindChandler` in `pkg/plan`).
- `source` carries provenance fields chandler populates and the
  hash excludes (pure metadata):

  ```json
  "source": {
    "kind": "chandler",
    "config_path": "examples/docker-ce/chandler.yaml",
    "git_commit": "abc123...",
    "git_date": "2026-05-25T12:34:56Z",
    "target": { "distro": "debian", "codename": "bookworm" },
    "fetched_keys": [
      {
        "slug": "docker-ce",
        "source_url": "https://download.docker.com/linux/debian/gpg",
        "fingerprint": "9DC858229FC7DD38854AE2D88D81803C0EBFCD88",
        "uid": "Docker Release (CE deb) <docker@docker.com>",
        "expiry": null
      }
    ]
  }
  ```

  `target` is the empty target object in simple mode (all fields
  empty strings); chandler does not omit it, so the JSON shape is
  stable across simple and matrix mode.

- `artifacts[].assets` is always `[]` (no GitHub-style assets).
- `artifacts[].arch` is always `"all"`.
- `build_plan.aux_files` carries every keyring `.gpg`,
  `.sources` file, and optional `postinst.sh` as base64 bytes.

The same `build_inputs_hash` formula applies. With `assets=[]`, the
canonical input is just `tool.format_revision` + the resolved
`build_plan` subtree, which covers every byte chandler stages. Vendor
key rotation flows into the hash through the keyring's `content_b64`
in `aux_files`; YAML edits flow through `nfpm` and the rendered
`.sources` bytes; `targets[]` edits flow through the resolved
`nfpm.name` and substituted source fields.

Chandler adds `error.kind` categories for key fetch/dearmor failures,
missing-git, and template-rendering errors. Exact strings are an
implementation choice (see `internal/chandler/discover/`).

### Plan format evolution

Chandler adds two fields to `plan.Source` — `Target` and `FetchedKeys`
— that cooper and staves leave zero. This is accepted as a non-breaking
additive change for v0; cooper and staves's `omitempty`-marshalled
output is unchanged.

A broader review of `pkg/plan`'s shape — whether source-kind-specific
fields belong on the shared `Source` struct vs. carved into per-kind
subtypes — is deliberately deferred until the trio has more tools
landed and the feature-creep signal is clearer. For v0, let it grow.

## CLI

```
chandler discover <CONFIG>  [-o OUTPUT]                # YAML → JSON plan
chandler validate <CONFIG>                              # parse + lint; no network
```

Positional argument required (use `-` for stdin). No implicit default
config path.

- **`discover <CONFIG>`** — reads the chandler YAML, fetches keys,
  emits a `plan.Plan` JSON document. `-o OUTPUT` writes to a file
  (`-` for stdout, the default). Network use is bounded: one HTTPS
  GET per unique key URL across all targets (results are cached
  per discover run, so the PG matrix with one key URL produces one
  HTTPS fetch regardless of how many targets share the key).

- **`validate <CONFIG>`** — lint without network. Checks: schema
  shape; key references resolve (every `sources[].key` matches a
  slug in `keys:`); no unused entries in `keys:`; source IDs unique
  and filename-safe; templating consistency (matrix vs no-matrix,
  package name template under matrix).

No `build` subcommand — `cooper build -` is the build phase.

No daemon mode. Discover runs from a developer's shell, a CI job, or
drayman. Drayman's role is the same as for cooper: query the target
apt repo by `X-Cooper-Build-Inputs-Hash`, drop already-imported
artifacts, pipe survivors through `cooper build`.

## Implementation stack

Pure Go, no CGO.

- **`github.com/ProtonMail/go-crypto/openpgp`** — armored→binary
  dearmor and fingerprint/UID/expiry extraction. ProtonMail's actively
  maintained fork of the deprecated `golang.org/x/crypto/openpgp`;
  drop-in replacement, same API. Avoids shelling out to `gpg
  --dearmor` so the discover phase stays hermetic and CI-portable.
- **`gopkg.in/yaml.v3`** — chandler.yaml parse + decode-with-strict
  unknown-key rejection.
- **`text/template`** — matrix-mode field templating.
- **`net/http`** — key URL fetches.
- **`pkg/plan`** (this module) — `Plan` types, JCS, build_inputs_hash.
  Chandler imports it the same way external producers would.

Chandler is **stateless**: no state files, no cross-run memory.

## Package layout

Chandler shares the apt-wharf Go module (`github.com/ophymx/apt-wharf`).
Following the conventions cooper and staves use, every chandler-internal
package lives under `internal/chandler/...`:

```
cmd/chandler/                 main, subcommand dispatch (validate, discover)
internal/chandler/config/     chandler.yaml parse/validation
internal/chandler/keys/       HTTPS fetch, dearmor, fingerprint/UID/expiry extraction
internal/chandler/render/     deb822 stanza rendering + postinst script rendering
internal/chandler/discover/   per-target orchestrator emitting plan.Plan
```

`pkg/plan` gains:

- `SourceKindChandler = "chandler"` constant.
- `Source.Target`, `Source.FetchedKeys` fields (chandler-only;
  cooper/staves leave them zero). Both are pure provenance; the
  hash already excludes everything under `source.*`.

## Security & sandboxing

Trust boundary: **HTTPS to the vendor key URL.** Inherits the system
CA store. No certificate pinning, no fingerprint requirement.
Documented risk: a compromised vendor cert chain at fetch time would
inline a bad key into the plan; the bad key would then ship to every
installer of that .deb. Acceptable tradeoff per
[[feedback-https-trust-anchor]].

Fetched key bytes are bounded by a response-size cap and an HTTP
timeout (constants in code, not spec). Dearmor failure is a discover
error; the binary blob is rejected without further parsing.

## Reproducibility

Chandler's reproducibility properties are inherited from cooper-build
plus one chandler-specific input: the dearmored keyring bytes. Two
chandler discover runs against the same YAML config, on the same
git commit, fetching the same key bytes from the vendor, produce
byte-identical `plan.Plan` JSON.

What's pinned chandler-side:

- `source_date_epoch` is the git commit time of the chandler YAML
  file. Not-in-git is a hard error.
- Dearmor is deterministic given the armored input (the OpenPGP
  binary serialization is canonical).
- deb822 stanza rendering writes fields in a fixed order:
  `Enabled`, `Types`, `URIs`, `Suites`, `Components`,
  `Architectures` (when present), `Signed-By`. No map iteration,
  no readdir, no wall-clock.
- `aux_files` keys are sorted lexically by `pkg/plan`'s JCS pass
  before `build_inputs_hash`.

What's pinned vendor-side (out of chandler's control):

- The bytes returned by the vendor's HTTPS endpoint. If the vendor
  rotates the key or adds a comment to the armored blob, chandler
  sees new dearmored bytes → new `aux_files` entry → new
  `build_inputs_hash`. Drayman auto-bumps the revision. This is
  the desired behavior — operators want to learn about vendor
  key rotation, not silently ignore it.

A CI self-check builds a fixed corpus of YAML configs twice
(`chandler discover` → `cooper build`) and diffs SHA256s. Lives at
`internal/chandler/discover/reproducibility_test.go`. Auto-skips
when nfpm isn't on PATH (same gate as cooper's e2e check).

## Examples directory

`examples/<vendor>/chandler.yaml` files double as copy-paste templates
and as the validator's regression corpus. Same flat layout cooper and
staves use (`examples/<name>/cooper.yaml`, `examples/<name>/staves.yaml`)
— each tool's `examples_test.go` globs for its own filename, so the
three example sets coexist without colliding.

`TestExamplesValidateClean` in `cmd/chandler/` sweeps every
`examples/*/chandler.yaml` through `chandler validate` and must come
back clean.

## Implementation status

Not yet implemented as of 2026-05-25. No `cmd/chandler/`,
`internal/chandler/`, or `examples/chandler/` exists in the tree.
This document is the spec.

## Deferred (post-v0)

- **GPG fingerprint pinning.** A `keys.<slug>.fingerprint:` field
  would let users opt into verifying the fetched key against an
  expected fingerprint, failing discover on mismatch. Deferred for
  v0 — HTTPS is the trust anchor and demanding fingerprints risks
  pushing users back to the curl|sh shell snippets chandler is
  designed to replace. Revisit if a hardened-deployment use case
  surfaces.
- **Local-file keys.** `keys.<slug>.file:` and `.files:` to source
  the keyring from a path relative to the chandler YAML, instead of
  fetching over HTTPS. Useful for in-house repos or air-gapped
  builds; defer until someone hits the use case.
- **Per-source `applies_to:` filtering.** When matrix has N targets
  but one source only makes sense on M < N of them, the workaround
  is to split into two YAMLs (separate .debs, separate apt
  installables). Better UX than a filter in many cases. Revisit only
  if a real-world template forces it.
- **Scoped postinst `apt-get update`.** Full update is simpler and
  less version-sensitive across apt releases; revisit if real users
  complain about full-update side effects.
- **Sigstore / signed-by-key verification.** A chandler config could
  carry a sigstore bundle or a separate signed manifest of expected
  key bytes; verifying that would harden the trust model beyond
  HTTPS. Significant new dependency surface; defer until at least
  one real consumer asks.
- **Inline `Signed-By:` armored keys.** deb822 supports embedding
  the armored key directly in the `.sources` file with continuation
  indentation, removing the need for a separate keyring file. Saves
  one dpkg file but complicates conffile semantics (the key bytes
  become part of the conffile, so vendor rotation prompts the
  operator). Stick with the separate-keyring layout for v0.
- **OS-detection install pattern.** A single `_all.deb` whose
  `.sources` file lists multiple codenames in `Suites:` and lets apt
  pick the matching one. Works only when URIs/Components are
  identical across distros, and apt warns noisily about unmatched
  codenames. The matrix model produces a per-codename .deb instead,
  which is the more honest UX. Revisit if a vendor specifically
  requests it.
