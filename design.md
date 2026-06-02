# apt-signpost — Design

## Problem

Several Debian packages we depend on are distributed as standalone `.deb`
downloads — the upstream vendors don't publish apt repositories. Today this is
patched over with hand-rolled scrapers feeding a `reprepro` repo, which means
we end up storing and re-serving bytes we don't need to host.

## Approach

Stand up an apt repository that:

- Hosts and signs the repository metadata (`InRelease`, `Release`,
  `Release.gpg`, `Packages`, `Packages.gz`, `Packages.xz`,
  `Packages.zst`).
- For `.deb` requests, **HTTP 302 redirects to the upstream URL** instead of
  hosting the bytes.
- Hashes each upstream `.deb` once (so `Packages` carries valid SHA256/size),
  and re-hashes whenever discovery says upstream changed.

apt follows redirects on `.deb` fetches and verifies their SHA256 against
`Packages`, so the redirect model is safe: a silently-mutated upstream URL
fails the hash check on the client (desired behavior), and the refresher's
job is to detect and re-publish before that happens.

A caching proxy sits in front of the signpost in deployment, so the signpost
itself does **not** cache `.deb` bytes.

## Trust model

Repo-level signing only.

- Sign `Release` → emit `InRelease` (inline clearsigned) and `Release.gpg`
  (detached).
- No per-`.deb` signing, no `debsigs`, no `Signed-By` per package.
- Trust chain: pubkey → `InRelease` signature → `Packages` hashes →
  `.deb` hashes.

## Bootstrap package

The repository hosts its own `<repo>-archive-keyring` package, built by the
signpost itself. Two package classes coexist:

| Class               | Source                | Hosted bytes        | In `Packages` |
| ------------------- | --------------------- | ------------------- | ------------- |
| Stitched (external) | upstream URL          | no — 302 redirect   | yes           |
| Bootstrap (internal)| built by signpost     | yes — real file     | yes           |

The bootstrap `.deb` ships:

- `/etc/apt/sources.list.d/<repo>.sources` (deb822 form, with
  `Signed-By: /usr/share/keyrings/<repo>-archive-keyring.gpg`)
- `/usr/share/keyrings/<repo>-archive-keyring.gpg` — exported public key(s)

It is rebuilt when: signing key changes, repo base URL changes, suite list
changes, or version is bumped. Architecture is `all`.

Versioning: `YYYY.MM.DD.N` — easy to reason about, no manual bumps.

Key rotation strategy: the bootstrap keyring ships **current + next** keys, so
a future rotation only requires switching the active signer after a soak
period — installed clients already trust the next key. Concretely, point
`signing.next_pubkey_file` at the public half of the upcoming key; the
loader checks that its fingerprint differs from the active key and bundles
both into the keyring.

For local trial / first-run UX, `signing.auto_generate: true` materializes
a fresh unencrypted RSA key at `signing.key_file` on first start. Production
deployments should generate the key offline and copy it in.

## URL layout

```
/dists/stable/InRelease
/dists/stable/Release[.gpg]
/dists/stable/main/binary-<arch>/Packages[.gz|.xz|.zst]

/pool/main/<...>/<name>_<version>_<arch>.deb       # external → 302 redirect
/pool/main/<...>/<repo>-archive-keyring_<v>_all.deb # internal → real file

# Bootstrap convenience endpoints
/<repo>-archive-keyring.gpg                         # raw public key
/release/<suite>/<arch>/latest.deb                  # 302 → versioned bootstrap .deb
/release/<suite>/<arch>/latest                      # plain text version string

# Observability (unauthenticated; ACL at the front proxy if exposed)
/status                                             # JSON: per-source state, tick, bootstrap
/metrics                                            # Prometheus text exposition
```

Suite support starts at just `stable`, but the `dists/<suite>/` layout is
preserved so adding more is non-structural.

## Implementation stack

Pure Go, no CGO.

- **`github.com/goreleaser/nfpm/v2`** — used as a library to build the
  bootstrap `.deb` at runtime. Signpost's own self-hosting `.deb` is
  produced by goreleaser (`.goreleaser.yaml`), which wraps nfpm.
- **`github.com/google/go-github/v86`** — GitHub Releases API client.
- **`github.com/tidwall/gjson`** — path expressions over JSON metadata
  responses for the `json_url` discoverer.
- **`pault.ag/go/debian`** — reading upstream `.deb` files and parsing
  control stanzas (saves rolling our own ar+tar+control parser).
- **`github.com/ProtonMail/go-crypto/openpgp`** — in-process clearsign for
  `InRelease` and detached sign for `Release.gpg`.
- **`compress/gzip`** + **`github.com/ulikunitz/xz`** +
  **`github.com/klauspost/compress/zstd`** — `Packages.gz`,
  `Packages.xz`, `Packages.zst`.
- **No database.** Per-source JSON state files on disk; the served
  repository is held in memory and published by an `atomic.Pointer` swap.

## Persistent state

```
state/
  sources/<source-id>.json           # latest known state per source
  bootstrap.json                     # input-hash + current version
  bootstrap/<version>.deb            # cached bootstrap .deb bytes
```

Per-source state captures: the discoverer's opaque change-detection token
(`discovery_token`), discovered URL, full control stanza for `Packages`,
asset size, asset SHA256, and last-checked / last-changed timestamps. For
github_release the `release_id` is also stored as a human-readable mirror
of the token.

These files are the **only** durable state. Everything served — `Packages`,
`Release`, `InRelease`, the bootstrap `.deb` bytes, the redirect map — is
held in memory and rebuilt from this directory at startup and on every
refresh tick.

State volume at expected scale (tens to low hundreds of sources) is well
under a megabyte. Single writer (refresher), so file locking is sufficient
— no transaction semantics needed.

## Runtime model

The served repository is one in-memory value, swapped atomically:

```go
type Snapshot struct {
    files     map[string][]byte  // metadata + bootstrap .deb bytes
    redirects map[string]string  // synthetic pool path → upstream URL
}

var current atomic.Pointer[Snapshot]
```

HTTP handler:

```go
snap := current.Load()
if data, ok := snap.files[path]; ok    { /* serve bytes */;  return }
if url,  ok := snap.redirects[path]; ok { /* 302 redirect */; return }
// 404
```

This gives one atomic visibility boundary for everything: a client request
captures `current` once at the top of the handler and sees a fully
consistent view of metadata + redirects for the rest of the request.

### Process states

```
   Booting ──► Idle ◄─┬─► Refreshing
                      │      │
                      │      ▼
                      └── (success: swap snapshot)
                          (failure: log, keep prior snapshot)
   Idle / Refreshing ──► Stopping
```

- **Booting**: parse config → load signing key → load `state/sources/*.json`
  → build initial snapshot (no network) → sign → `current.Store(snap)` →
  start HTTP → schedule refresh ticker → kick off first refresh
  asynchronously. Clients can install/upgrade against cached state
  immediately, before the first network refresh completes.
- **Idle**: ticker armed, HTTP serving `current`.
- **Refreshing**: single-writer mutex held; a tick that fires while held
  is dropped (logged), not queued.
- **Reloading** (SIGHUP): re-read config, validate the diff against
  hot-reloadable rules, build a fresh signer + discoverers + secrets, stop
  the old refresh loop, swap in the new "generation," reseed the status
  tracker, run a synchronous import + refresh, then start the new loop.
  The HTTP listener and the in-memory snapshot survive the reload — if
  the new tick fails, the prior snapshot keeps serving.
  Reload-rejecting fields (require process restart): `server.listen`,
  `paths.state_dir`. Everything else (sources, signing key contents,
  refresh interval, github tokens, repository identity) hot-swaps on the
  next tick.
- **Stopping**: stop accepting new requests, let an in-flight refresh
  complete (bounded by config timeout), then exit.

### Refresh-tick phases

```
[acquire refresh lock]
  1. Load state/sources/*.json into memory.
  2. Discovery sweep (bounded parallel):
       for each source:
         probe = disc.Probe(ctx, prev)
         if probe.Token == prev.Token: skip
         else: fetch → stream-hash → parse control →
               write state/sources/<id>.json (tmp + rename)
         on error at any sub-step: log, leave prior state file untouched.
  3. Bootstrap rebuild check (input-hash compare):
       inputs = base_url ⊕ bootstrap.* ⊕ suite.* ⊕ keyring-bytes
       if hash(inputs) == state/bootstrap.json.input_hash: reuse cached .deb
       else: nfpm-build, bump version, persist state/bootstrap.json + .deb.
  4. Build new snapshot:
       per-source stanzas → Packages → gzip/xz/zstd
       per-suite Release referencing those file hashes
       sign → InRelease (clearsigned) + Release.gpg (detached)
       compose files map + redirects map.
  5. current.Store(newSnapshot).
[release refresh lock]
```

### Atomicity guarantees

- **Per-source state file**: written via tmp-file + `rename()`, atomic on
  POSIX. Crash mid-write leaves prior file intact.
- **`.deb` bytes during fetch**: stream-hashed, never persisted (caching
  proxy in front owns bytes). Crash mid-fetch leaves no orphans; next tick
  re-attempts.
- **Snapshot publish**: a single `atomic.Pointer.Store`. There is no window
  where served metadata and the redirect map disagree.

### Failure isolation

| Failure                                  | Effect                                              |
| ---------------------------------------- | --------------------------------------------------- |
| Discovery error for source X             | Source X holds at last-known good; others ok       |
| Fetch / hash / control-parse error for X | Same                                                |
| Token unchanged for X                    | Skip fetch (intended)                               |
| Bootstrap rebuild fails                  | Whole tick fails, prior snapshot retained           |
| Signing fails                            | Whole tick fails, prior snapshot retained           |
| Process crash mid-tick                   | Restart rebuilds last-known-good snapshot from disk |

### `Valid-Until` policy

Omit `Valid-Until` from `Release` in v1. apt then imposes no freshness
requirement, so a signpost outage doesn't break installs. Revisit if we
want freshness as an explicit watchdog signal later.

## Discovery plugins

Single interface, trivially small:

```go
type Probe struct {
    URL   string  // resolved upstream .deb URL
    Token string  // opaque change-detection token
}

type Discoverer interface {
    Probe(ctx context.Context, prev *Probe) (*Probe, error)
}
```

Discovery does **only** "what URL, has it changed?" Version, hashes, and
control fields all come from introspecting the downloaded `.deb`. Refresher
short-circuits when `cur.Token == prev.Token`.

### Built-in: `latest_url`

```yaml
discovery:
  type: latest_url
  url: https://example.com/downloads/foo-latest.deb
```

- HTTP `HEAD` with redirect-follow.
- Token preference order: final resolved URL → `ETag` → `Last-Modified`.
- If none of those are available, token is empty and refresher always fetches
  (cheap behind the caching proxy).
- The HEAD also sends `x-amz-checksum-mode: ENABLED`. If the resolved host
  is on AWS S3 (`*.amazonaws.com` with an `s3` label) and the response
  carries `x-amz-checksum-sha256`, that value is base64-decoded into the
  Probe's asset digest so the refresher can skip the streaming hash.
  Non-S3 hosts and malformed checksums silently fall back to streaming.

### Built-in: `github_release`

```yaml
discovery:
  type: github_release
  repo: owner/name
  asset: 'foo_.*_amd64\.deb'
  include_prerelease: false
```

- `GET /repos/<repo>/releases/latest` (or list+filter for prereleases).
- Token = release `id`; URL = matching asset's `browser_download_url`.
- Sends `If-None-Match` with cached API `ETag` — most ticks return 304.
- Per-credential token bucket honors `X-RateLimit-*` headers; the
  unauthenticated bucket is shared across sources without a token.
- Optional GitHub token via `discovery.token_env` / `discovery.token_file`
  (per-source) or the global `github.token_env` / `github.token_file`.

### Built-in: `json_url`

```yaml
discovery:
  type: json_url
  url: https://zoom.us/rest/download?os=linux
  token_path: result.downloadVO.zoom.version
  asset_url: https://zoom.us/client/{token}/zoom_amd64.deb
```

- HTTP `GET` against the metadata endpoint; response body is capped at 1 MiB.
- `token_path` is a [gjson](https://github.com/tidwall/gjson) path resolved
  against the response. Its string form becomes the change-detection token.
- `asset_url` is a URL template. `{token}` substitutes the resolved
  `token_path` value; any other `{gjson.path}` interpolates an arbitrary
  field from the same response. The rendered URL must be `http`/`https` or
  the probe fails (defense against `file://` from a hostile JSON).
- Absorbs vendor download endpoints that publish JSON metadata
  (Discord, Slack, Zoom, Cypress, Postman, ...) without needing an
  external shim binary. `examples/external-producers/discover-zoom`
  is now redundant for the Zoom case — the same mapping fits in five
  lines of YAML.

### Built-in: `xml_url`

```yaml
discovery:
  type: xml_url
  url: https://www.jetbrains.com/updates/updates.xml
  token_xpath: "//product[@name='IntelliJ IDEA']/channel[@status='release']/build/@number"
  asset_url: "https://download.jetbrains.com/idea/ideaIC-{token}.tar.gz"
```

- The XML counterpart to `json_url`. HTTP `GET` against the
  metadata endpoint; response body is capped at 1 MiB and parsed
  with `github.com/antchfx/xmlquery`.
- `token_xpath` is an XPath expression resolving to the
  change-detection token (typically the version string).
- `asset_url` is a URL template. `{token}` substitutes the resolved
  `token_xpath` value; `{xpath:<expr>}` runs any XPath against the
  same response body. The `xpath:` prefix is required to keep the
  surrounding URL grammar parseable (raw XPath includes `[`, `]`,
  `/`, `=`). The rendered URL must be `http`/`https` or the probe
  fails.
- Targets JetBrains' `updates.xml` (the canonical case), Apache
  project release feeds, and RSS / Atom-shaped download indexes.

### Built-in: `external`

**When to reach for this.** `external` is the last-resort discovery
kind. Reach for it only when the vendor's metadata is structurally
unrepresentable in `json_url` / `xml_url` — a multi-step auth
handshake, cookie-based session state, paginated listings the
built-ins can't follow, or a non-JSON/XML wire format. If the vendor
publishes its release metadata as a single JSON or XML document you
can `GET`, the declarative kinds win on transparency and don't
require shipping a separate binary. The Zoom case used to be the
poster child for this kind; it now lives in five lines of `json_url`
config (see *Built-in: json_url* above) — the redundant
`examples/external-producers/discover-zoom` binary remains only as a
worked reference for the stdio contract.

Escape hatch for vendors with bespoke discovery needs. Language-agnostic
contract over stdio.

```yaml
discovery:
  type: external
  command: ["/usr/local/libexec/discover-baz", "--channel", "stable"]
  timeout: 30s
```

- signpost executes `command` with a clean environment (allowlist:
  `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` and their lowercase variants,
  plus per-source `env` config). `command[0]` must be an absolute path
  because PATH is not in the allowlist.
- **stdin**: one JSON object — `{"prev": {"url": "...", "token": "..."}}`
  (`prev` omitted on cold start).
- **stdout**: one JSON object — either `{"url": "...", "token": "..."}` or
  `{"unchanged": true}`.
- **stderr**: free-form, captured into signpost logs at DEBUG on success
  and INFO on error; the trailing 1 KiB also rides the returned error.
- **exit code**: 0 success, non-zero error (source holds at previous state,
  error logged).
- Hard kill on timeout: the child runs in its own process group on unix
  and the entire group is SIGKILLed when `timeout` elapses, so wrapper
  shells can't shield long-running grandchildren. `WaitDelay` caps how
  long Wait can hang on dangling pipes as a belt-and-suspenders.

External commands return discovery metadata only. They do **not** download
the `.deb` themselves — the refresher owns all HTTP, hashing, and error
handling. If a vendor needs auth headers to fetch, that goes in a separate
fetch-options config, not in the script.

#### Secrets convention

For external sources, signpost never needs the secret content — only the
external tool does. The convention is to pass **file paths** through
`env:`, not values:

```yaml
env:
  BAZ_TOKEN_FILE: "/var/lib/signpost/secrets/baz.token"
```

This keeps secrets out of signpost's memory, the child's `/proc/<pid>/environ`,
and any structured logs that capture exec arguments. signpost doesn't enforce
the convention because non-secret env values (e.g. `BAZ_REGION: us-west-2`)
are still legitimate.

#### Go helper package

Tools written in Go can import `github.com/ophymx/apt-wharf/external` for
the wire-format types (`Input`, `Probe`, `Output`), `Output.Validate`, and a
`Run(ProbeFunc) error` helper that wires `os.Stdin`/`os.Stdout` to a
SIGINT/SIGTERM-cancellable context.
`examples/external-producers/discover-zoom/` ships as a working
example covering the full contract.

## GPG / signing

Configurable, but always non-interactive at runtime. Two top-level modes,
selected by which `signing.*` block is set in config (mutually exclusive
at validation):

### Internal — in-process via go-crypto

- **Unencrypted key file** — config points at the secret key file; tool
  loads it on startup. Startup checks file mode `0400` and ownership.
- **Passphrase-protected key file** — passphrase from env var or a file
  (mode `0400`). Loaded once at startup, key kept unlocked in memory for the
  lifetime of the process.

Signing is in-process via `github.com/ProtonMail/go-crypto/openpgp`. No
external `gpg` binary, no `gpg-agent`.

### External — exec a CLI per signing op

For Vault-backed or HSM-backed signing the daemon delegates to a
configurable command instead of holding secret material itself. The
contract is a fixed subset of `vault-pgp-sign(1)` so that binary wires in
directly; everything else fits behind a thin shell shim (see
`packaging/contrib/vault-pgp-sign-shim.sh` for the worked example).

Per signing op, signpost runs:

```
<command...> {detach-sign|clear-sign} [--key <name>]
    --in <staged-input> --out <staged-output>
    [--source-date-epoch <unix>] [--digest-algo SHA256]
```

stdin is unused; stderr is captured into signpost logs (DEBUG on
success, INFO on error) and the trailing 1 KiB rides any error. The
child runs in its own process group with a hard SIGKILL on timeout
(same discipline as the external discovery contract).

The bootstrap keyring + `/pubkey.gpg` content come from one of:

1. `signing.external.pubkey_file` — operator-supplied armored or binary
   pubkey, read at startup.
2. `<command> export --key <name>` — run once at startup when
   `pubkey_file` is unset; stdout is parsed as an OpenPGP cert.

One of the two must succeed for the daemon to come up. Both forms are
canonically re-serialized so the bootstrap input hash stays stable
across reload — apt must not see a phantom upgrade when signpost
restarts.

`signing.next_pubkey_file` is honored in both modes (rotation soak
period). Smartcard / hardware-key support is achieved in this mode via
the external command — signpost itself has no opinion on how the child
acquires its signing material.

## Package layout

```
cmd/signpost/         main, flag/config wiring
external/             public Go helper for tools implementing the external contract
examples/external-producers/discover-zoom/
                      reference external discoverer (Zoom Linux client; see
                      §"Built-in: external" for when to reach for this pattern)
internal/config/      YAML schema, validation, env interpolation
internal/source/      Discoverer interface + built-in impls (github_release, latest_url, json_url, xml_url, external)
internal/refresh/     poll loop, change detection, control extraction, snapshot composition
internal/index/       Packages, Release, InRelease writers
internal/sign/        Signer interface + internal (go-crypto) and external (exec) backends
internal/bootstrap/   nfpm-driven keyring/.sources package builder
internal/store/       per-source JSON state read/write
internal/fetch/       range-fetch + control extraction from upstream .debs
internal/server/      http: static metadata, redirector, /release/... endpoints
packaging/            systemd unit and signpost's maintainer scripts
                      (referenced by .goreleaser.yaml at the repo root)
packaging/contrib/    optional adapters for the external signing contract
                      (vault-pgp-sign-shim.sh today)
```

## Configuration

YAML, loaded once at startup. A complete annotated example lives at
[`config.example.yaml`](config.example.yaml); this section covers the
schema semantics that aren't obvious from the example.

### Top-level shape

```
repository:    # Origin/Label/base_url for the published Release file and bootstrap .sources
suite:         # the single served suite: { codename, description, architectures }
bootstrap:     # bootstrap .deb metadata (package_name, maintainer, description)
signing:       # key_file (+ optional auto_generate, next_pubkey_file, passphrase_*)
server:        # listen address
refresh:       # interval, jitter, http_timeout
github:        # global github token + rate-limit budget shared across github_release sources
paths:         # state_dir
sources:       # map of source name → per-source config (see below)
```

The MVP serves a single suite; the URL layout (`/dists/<codename>/...`)
preserves room for adding more without restructuring. Components are not
configurable in v1 — every package lands under `main`.

### Per-source shape

```
sources:
  <source-name>:                   # key matches ^[a-z0-9][a-z0-9._-]*$
    enabled: true                  # default
    discovery: { type: ..., ... }  # required, discriminated union (below)
```

Sources are a map keyed by name (not a list with an `id` field) — the YAML
parser rejects duplicates in strict mode. The Debian package name, version,
architecture, and full control stanza all come from introspecting the
downloaded `.deb` — they are not declared in config. The map key is purely
an internal handle for state filenames and pool paths.

`sources:` must contain at least one entry; signpost refuses to start
otherwise (this is the desired fail-fast behavior on a freshly-installed
default config).

### Discovery union

`discovery.type` is the discriminator; the other keys under `discovery` are
validated against the matching variant. In Go terms:

```go
type LatestURL struct { URL string }

type GitHubRel struct {
    Repo string; Asset string         // asset is a Go regex over asset names
    IncludePrerelease bool
    TokenEnv string                   // optional; overrides github.token_env
    TokenFile string                  // optional; mutually exclusive with TokenEnv
}

type External struct {
    Command []string
    Timeout time.Duration             // default 30s when omitted
    Env     map[string]string         // explicit allowlist layered on top of HTTP_PROXY/HTTPS_PROXY/NO_PROXY
}
```

In the actual implementation, `Discovery` is a single flat struct holding
the union of fields; the validator branches on `type` and rejects fields
that don't belong. Unknown `type` values fail config validation at startup.

### Env var interpolation

Any string value supports `${VAR}` expansion at config load. Required for
tokens that should not sit in plaintext config (`GITHUB_TOKEN`, vendor
download tokens, external-command env). Missing vars fail loud at startup,
not silently to empty string.

Scope: applied to the raw config bytes before YAML decode, so it is uniform
across every value — including `discovery.external.command` argv. Tools that
want literal `${VAR}` strings should pass them through `env:` rather than
embedding them in command arguments.

### Validation rules (enforced at startup, hard-fail)

- `repository.base_url` is a valid `http`/`https` URL with no path or
  trailing slash.
- `suite.codename` matches `^[a-z0-9][a-z0-9._-]*$`; `suite.architectures`
  is non-empty and does not contain `"all"` (handled implicitly).
- `bootstrap.package_name` matches Debian's package-name grammar.
- `signing.key_file` is an absolute path. When `signing.auto_generate` is
  false (the default), the file must exist and pass the secure-file check
  (mode `0400`, owned by the service user). With `auto_generate: true`,
  the check is deferred until after first-start key materialization.
- `signing.passphrase_env` and `signing.passphrase_file` are mutually
  exclusive; the file (when used) passes the secure-file check.
- `signing.next_pubkey_file` (when set) is absolute; fingerprint distinctness
  vs. the active key is enforced at key-load time.
- `signing.external` and the internal-mode fields (`signing.key_file`,
  `signing.passphrase_env`, `signing.passphrase_file`, `signing.auto_generate`)
  are mutually exclusive. When `signing.external` is set: `command` is
  non-empty; `command[0]` is absolute, exists, is not a directory, and is
  executable; `timeout >= 0`; `pubkey_file` (when set) is absolute and
  exists; `env` keys do not contain `=` or `\0`.
- `server.listen` is non-empty.
- `refresh.interval > 0`, `refresh.jitter >= 0`, `refresh.http_timeout > 0`.
- `github.token_env` and `github.token_file` are mutually exclusive; the
  file (when used) passes the secure-file check.
- `paths.state_dir` is an absolute path.
- `sources` has at least one entry.
- Each source map key matches `^[a-z0-9][a-z0-9._-]*$`. Uniqueness is
  enforced by the YAML parser (strict mode rejects duplicate keys).
- `discovery.type` is one of `github_release`, `latest_url`, `external`.
  Type-specific fields belonging to other variants are rejected.
- For `latest_url`: `url` is a valid http(s) URL with a host.
- For `github_release`: `repo` matches `^[^/]+/[^/]+$`; `asset` compiles
  as a Go regex; `token_env`/`token_file` are mutually exclusive.
- For `external`: `command` is non-empty; `command[0]` is absolute, exists,
  is not a directory, and has at least one executable bit. `timeout >= 0`.
  `env` keys do not contain `=` or `\0`.

### Intentionally not configurable in v1

- Per-source refresh interval — global only.
- Output compression formats — emit `Packages`, `Packages.gz`,
  `Packages.xz`, and `Packages.zst`. All four are hashed into
  `Release` and laid down at the canonical path plus by-hash entry,
  so apt picks whichever its `CompressionTypes::Order` prefers.
- Hash algorithms in `Packages` and `Release` — SHA256 + Size only.
  No SHA1, no MD5. Modern apt (Debian 10+ / Ubuntu 18.04+) verifies
  against SHA256; older clients are unsupported.
- Bootstrap file layout — generated, not user-templated.

## Open questions

- **Operational surface** — CLI for "force refresh source X", "rotate
  key"? Today there is `signpost check --source NAME` for one-off
  probe+fetch verification, `signpost serve` for the daemon, and `kill
  -HUP <pid>` for reload+refresh-now. No per-source force-refresh, no
  key-rotate command.
- **Webhook-triggered refresh** — GitHub releases can webhook signpost to
  cut time-to-publish from up-to-an-hour to seconds; introduces an
  authenticated-write surface that doesn't exist today.
- **Upstream signature verification** — the trust chain currently bottoms
  out at "we hashed what was at the URL." Vendor PGP detached sigs or
  sigstore bundles, when available, would close the "compromised vendor"
  gap.
- **Multi-suite support** — the on-disk URL layout (`/dists/<codename>/`)
  preserves room, but the config schema is single-suite (`suite:`). Promote
  to a map when there's a concrete need.
- **Multi-component support** — every package currently lands under `main`.
  Same story: layout-ready, schema-deferred.

Resolved since the initial design:

- **Bootstrap keyring contents during rotation** — current + next, exposed
  as `signing.next_pubkey_file`. Loader rejects same-fingerprint pubkeys.
- **`Architecture: all` packaging** — fans into every
  `binary-<arch>/Packages` listed in `suite.architectures`; no separate
  `binary-all` listing.
- **`/status` and `/metrics`** — `/status` returns JSON with per-source
  last-checked / last-changed / last-error / current package + version,
  bootstrap version + last-built timestamp, and tick counters. `/metrics`
  is Prometheus text exposition over the same data plus snapshot-derived
  gauges. Both unauthenticated; ACL at the front proxy when exposed.
- **SIGHUP reload + immediate refresh** — see *Process states / Reloading*
  above. Most fields hot-swap; `server.listen` and `paths.state_dir`
  changes are rejected and logged.
