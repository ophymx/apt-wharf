# apt-signpost MVP — Design

A trimmed v0 focused on one job: stitch GitHub release `.deb` assets into a
signed apt repository, with the smallest possible per-source bandwidth and
state. The full vision lives in [`design.md`](design.md); this document is
what we actually ship first.

## Scope

In:

- One discovery source: GitHub releases.
- One suite (`stable`), one component (`main`), N architectures.
- Range-fetch only the head of each `.deb` to extract the control stanza —
  we never download `data.tar.*`.
- Trust GitHub's API-supplied SHA256; fall back to streaming-hash on the
  rare assets where it isn't exposed.
- SHA256-only hashes in `Packages` and `Release`. No MD5, no SHA1.
- `Acquire-By-Hash: yes`.
- 302 redirect to upstream for every external `.deb` request.
- Repo-level signing → `InRelease` + `Release.gpg`.
- Self-built bootstrap keyring `.deb` (via nfpm), so install is one
  `curl | dpkg -i` instead of hand-editing `/etc/apt/sources.list.d/`.

Deferred to v1+:

- `latest_url` and `external` discovery.
- Multiple suites / components.
- `xz` compression.
- Append-only history log.
- Per-source refresh interval, `/status` endpoint, force-refresh CLI.

## Approach

Same redirect model as the full design — apt fetches `Packages` from us,
follows our 302 to the upstream `.deb`, and verifies SHA256 against our
signed metadata. The MVP-specific change is **how** we obtain that SHA256
and the control stanza:

1. **Discovery**: hit the GitHub releases API with `If-None-Match`
   (cached `ETag`). On 304, skip. On 200, the release `id` is the change
   token; the matching asset's `browser_download_url` is the upstream URL.
2. **Hash**: read the asset's `digest` field (`sha256:<hex>`) — populated
   on virtually all modern release assets. If missing, fall back to a full
   streaming GET that hashes as it reads.
3. **Control stanza**: HTTP `Range: bytes=0-1048575` (1 MB) on the asset.
   Parse the `ar` archive in the buffer, locate `control.tar.{gz,xz,zst}`,
   decompress, extract the inner `control` file. If the buffered window
   doesn't cover the full control archive, issue one precise follow-up
   range for exactly the missing bytes.

apt verifies the redirected `.deb` against `Packages`, so trusting GitHub's
`digest` is no riskier than trusting the upstream URL itself: a tampered
asset fails the client check (desired) and the next refresh re-publishes.

## Trust model

- Repo-level signing only — same as the full design.
- Pubkey → `InRelease` signature → `Packages` SHA256 → `.deb` SHA256.
- `Packages` lists **only** SHA256. `Release` lists each metadata file's
  SHA256 only. No MD5, no SHA1. Modern apt (Debian 10+, Ubuntu 18.04+) is
  fine with this; pre-Stretch clients are out of scope.

## URL layout

```
/dists/stable/InRelease
/dists/stable/Release
/dists/stable/Release.gpg
/dists/stable/main/binary-<arch>/Packages
/dists/stable/main/binary-<arch>/Packages.gz
/dists/stable/main/binary-<arch>/by-hash/SHA256/<hex>     # Acquire-By-Hash
/pool/main/<p>/<name>_<version>_<arch>.deb                # 302 → upstream

/pool/main/<p>/<keyring>_<v>_all.deb                      # bootstrap (real bytes)
/release/stable/latest.deb                                # 302 → versioned bootstrap
/release/stable/latest                                    # text: bootstrap version
/pubkey.gpg                                               # binary public key (convenience)
```

The by-hash path is a parallel view of `Packages`/`Packages.gz`: a client
that sees `Acquire-By-Hash: yes` in `Release` may fetch by hash instead of
by name. The snapshot maps both the canonical path and the by-hash path to
the same bytes.

The bootstrap `.deb` ships:

- `/usr/share/keyrings/<package_name>.gpg` — multi-key binary keyring
  containing the active pubkey and any pre-positioned next pubkey
  (see [Key rotation](#key-rotation)).
- `/etc/apt/sources.list.d/<package_name>.sources` — deb822 form, with
  `Signed-By:` pointing at the keyring path above and `URIs:` set from
  `repository.base_url`.

Architecture is `all`, so the stanza appears in every
`binary-<arch>/Packages`. (MVP duplicates `all`-arch stanzas into each
`binary-<arch>` rather than maintaining a separate `binary-all`
component — apt accepts both, this is simpler.) Versioning is
`YYYY.MM.DD.N` (auto-bumped on input change). `/pubkey.gpg` stays as a
convenience for users who want to inspect or trust-on-first-use
without installing the keyring package; its contents match the
bootstrap keyring file (active pubkey, plus any pre-positioned next
pubkey during rotation).

Install UX:

```
curl -fsSL https://apt.acme.example/release/stable/latest.deb -o /tmp/acme.deb
sudo dpkg -i /tmp/acme.deb
sudo apt update
```

## Implementation stack

Pure Go 1.21+, no CGO.

- **`pault.ag/go/debian`** — control-stanza parsing. We hand it a
  `bytes.Reader` over the in-memory control archive, not a file handle.
- **`github.com/ProtonMail/go-crypto/openpgp`** — clearsign for
  `InRelease`, detached sign for `Release.gpg`.
- **`compress/gzip`** — `Packages.gz`.
- **`github.com/goreleaser/nfpm/v2`** — building the bootstrap `.deb`.
- **`log/slog`** (stdlib) — structured logging. All ERROR/WARN/INFO
  events emitted as JSON with stable fields (`source_id`, `bucket_key`,
  `release_id`, etc.).
- **No `xz`** dependency.
- **No database** — per-source JSON state on disk; in-memory snapshot
  swapped via `atomic.Pointer`.

## State on disk

```
state/
  sources/<id>.json          # per upstream source
  bootstrap.json             # input-hash + current bootstrap version
  bootstrap/<version>.deb    # cached bootstrap .deb bytes
```

### Per-source state file

```json
{
  "id": "vendor-bar-amd64",
  "release_id": 12345678,
  "release_tag": "v1.2.3",
  "asset_url": "https://github.com/.../bar_1.2.3_amd64.deb",
  "asset_size": 12345678,
  "asset_sha256": "abc...",
  "api_etag": "W/\"...\"",
  "control": "Package: bar\nVersion: 1.2.3\nArchitecture: amd64\n...\n",
  "last_checked": "2026-05-09T10:30:00Z",
  "last_changed": "2026-05-09T10:30:00Z"
}
```

The verbatim `control` blob is what gets emitted into `Packages` (with
`Filename`, `Size`, `SHA256` appended). Storing it raw means startup is
zero-network: we read state, build snapshot, sign, serve.

### Bootstrap state file

```json
{
  "version": "2026.05.09.1",
  "input_hash": "sha256:...",
  "filename": "acme-archive-keyring_2026.05.09.1_all.deb",
  "size": 12345,
  "sha256": "..."
}
```

`input_hash` covers everything that should force a rebuild: `base_url`,
all `bootstrap.*` config fields, suite codename + components +
architectures, and the raw bytes of every public key being shipped
(active + optional next, in canonical order — see
[Key rotation](#key-rotation)). The `.deb` itself is cached at
`state/bootstrap/<version>.deb` and reused verbatim across restarts.

## Snapshot

```go
type fileEntry struct {
    data      []byte
    expiresAt time.Time   // zero == never expires
}

type redirect struct {
    url       string
    expiresAt time.Time   // zero == never expires
}

type Snapshot struct {
    files     map[string]fileEntry  // metadata, by-hash, bootstrap, pubkey
    redirects map[string]redirect   // pool paths → upstream URLs, latest.deb
}

var current atomic.Pointer[Snapshot]
```

HTTP handler logic: `snap := current.Load()` once at the top of the
request, then file lookup → serve bytes, redirect lookup → 302, else
404. Expiry is enforced only at snapshot composition time, not on the
request path — once an entry is in a published snapshot it serves until
the next swap, even if its `expiresAt` has passed.

## Atomic update across requests

The single `atomic.Pointer.Store` gives us per-request consistency, but
an `apt update` is a *sequence* of requests. The race we have to defend
against is: client fetches `InRelease` at T0 (sees `Packages.gz` hash
H1), we publish a new snapshot at T1, client fetches `Packages.gz` at T2
and gets H2 bytes. Hash check fails, apt errors with "Hash Sum
mismatch".

Defense is `Acquire-By-Hash: yes` (already in our `Release`) plus
**retention**:

1. Every metadata file is published at both its canonical path
   (`.../Packages.gz`) and its by-hash path
   (`.../by-hash/SHA256/<hex>`).
2. Clients with Acquire-By-Hash on (apt default since 1.5 — Buster /
   Bionic era) fetch by hash. An `InRelease` pinned to H1 always asks
   for `.../by-hash/SHA256/H1`, never the canonical name.
3. When we publish snapshot N+1, snapshot N's by-hash entries stay
   reachable for a grace window so an in-flight `apt update`
   completes against the snapshot it started with.

Same logic for the pool redirect map: if a source's version bumps
mid-install, the client's stale `Packages` still names
`foo_1.0.0_amd64.deb`, and we need that pool path → old upstream URL
mapping retained for the same window.

Composition rule when building snapshot N+1 from `prev = N`:

- New canonical metadata (`InRelease`, `Release`, `Release.gpg`, the
  canonical `Packages` and `Packages.gz` paths), `/release/stable/latest`
  (text), `/pubkey.gpg` → `expiresAt = zero`. These are always
  overwritten in the next snapshot, so retention is meaningless.
- New by-hash entries, new pool redirects (upstream `.deb`s), AND the
  bootstrap `.deb`'s versioned pool path → `expiresAt = now + retention`.
  These are version- or hash-keyed, so older snapshots' entries must
  survive the grace window for in-flight installs to complete. Without
  this, a client mid-install on stale `Packages` referencing the prior
  bootstrap version hits 404.
- For every `(path, entry)` in `prev.files` and `prev.redirects` where
  `entry.expiresAt > now` and the path is **not** already present in
  the new snapshot → carry it over verbatim. This cascades: an entry
  minted at T0 survives every subsequent snapshot until T0 + retention,
  regardless of refresh cadence.

Retention default 30 minutes. apt finishes a normal update + install in
seconds; 30 min covers slow links, paused laptops, and human typing.
Not configurable in MVP.

Clients with `Acquire-By-Hash=false` (some sites disable it) still race
during the swap. Cost is one "Hash Sum mismatch" and a manual `apt
update` retry. Acceptable.

## Startup

Phases, in order:

1. Parse + validate config. Hard-fail on any validation rule violation,
   missing `${VAR}` interpolation, malformed regex, etc.
2. Load signing key file (and `next_pubkey_file` if set).
3. Load `state/sources/*.json` and `state/bootstrap.json` into memory.
4. **Synchronous import of new sources.** For every source declared in
   config whose `state/sources/<id>.json` is absent, run the same
   pipeline as a refresh-tick fetch (discovery probe → range-fetch →
   control parse → SHA256 verify) in bounded parallel. Any failure is
   fatal — the daemon exits with a per-source error report. On success,
   write state files (tmp + rename) before proceeding.
5. Build initial snapshot from all state files; bootstrap rebuild
   check; sign; `current.Store(snap)`.
6. Start HTTP listener.
7. Schedule the refresh ticker; kick off the first async refresh.

Existing sources never block startup on network — they're served from
their cached state file, and the async refresher handles freshness. Only
genuinely new sources gate startup, which is exactly the moment when
the operator wants confirmation that the new config is reachable and
valid. Drift on existing sources (regex changed, repo renamed) is **not**
detected synchronously in MVP; the next async refresh catches it and
logs ERROR. To force a fresh import on restart, delete the source's
state file first.

## Shutdown

On SIGTERM or SIGINT, stop accepting new HTTP connections, wait for
any in-flight refresh tick to complete (bounded by
`refresh.http_timeout`), best-effort zero of in-memory token bytes and
the unlocked signing key, exit 0. A second signal during shutdown
forces immediate exit (non-zero).

## Refresh tick

```
[acquire single-writer lock]
  1. Load state/sources/*.json into memory.
  2. For each source (bounded parallel):
       a. GitHub API GET /repos/<repo>/releases/latest with If-None-Match.
       b. If 304 or release id unchanged: skip.
       c. Match asset by anchored regex. Zero or multiple matches →
          source error (ERROR log, hold prior state).
       d. SHA256: prefer asset.digest. If missing, mark for streaming.
       e. Range-fetch bytes 0..1MiB.
              parse ar → find control.tar.*
              if member size <= bytes-on-hand: extract + decompress
              else: second Range for exact member span, extract
       f. If digest was missing: stream full asset, hash, verify size.
       g. Parse control stanza.
       h. Write state/sources/<id>.json (tmp + rename).
  3. Bootstrap rebuild check (input-hash compare):
       inputs = base_url ⊕ bootstrap.* ⊕ suite (codename, components,
                architectures) ⊕ pubkey-bytes (active + optional next,
                in canonical order)
       if hash(inputs) == bootstrap.json.input_hash:
           reuse cached state/bootstrap/<v>.deb
       else:
           bump version (YYYY.MM.DD.N), nfpm-build,
           write state/bootstrap/<v>.deb (tmp+rename),
           write state/bootstrap.json (tmp+rename).
  4. Pre-assembly strict checks (any failure → source-level ERROR,
     source held at prior state, excluded from this tick's snapshot):
       - Parsed Architecture must be "all" or in suite.architectures.
         Anything else (e.g., parsed "i386" with suite [amd64,arm64])
         is rejected — no silent drop.
       - No two sources may resolve to the same
         (Package, Version, Architecture) triple. All conflicting
         sources error together; none win.
  5. Per arch: assemble Packages from all sources whose parsed
     Architecture is <arch> or "all". Include the bootstrap stanza
     (Architecture: all) in every binary-<arch>/Packages.
       gzip → Packages.gz
       compute SHA256 of both → register canonical path AND by-hash path.
  6. Build Release: Origin, Label, Suite, Codename, Components,
     Architectures, Description, Date (RFC 5322, UTC),
     Acquire-By-Hash: yes, SHA256 list of every metadata file
     (Packages and Packages.gz per arch).
  7. Sign → InRelease (clearsigned), Release.gpg (detached).
  8. Compose new Snapshot:
       files     = metadata + by-hash + bootstrap .deb bytes
                   + /release/stable/latest (text) + /pubkey.gpg
       redirects = upstream pool paths → upstream URLs
                 + /release/stable/latest.deb → /pool/.../<keyring>_<v>_all.deb
       Then merge in non-expired entries from prev that aren't already
       present (see Atomic update across requests).
  9. current.Store(newSnapshot).
[release lock]
```

A tick that fires while a prior tick holds the lock is dropped (logged),
not queued. Errors on individual sources isolate to that source — others
continue, prior state file untouched on failure.

## Range fetch — details

The `ar` format is trivial:

- 8-byte global magic: `!<arch>\n`
- Each member: 60-byte header (16 name, 12 mtime, 6 uid, 6 gid, 8 mode,
  10 size, 2 trailer `\x60\x0a`) followed by `size` bytes of data, padded
  to even length with `\n`.

Member order is fixed by Debian policy: `debian-binary` (≈4 bytes), then
`control.tar.{gz,xz,zst}`, then `data.tar.*`. We need only the first two.

Strategy:

1. `Range: bytes=0-1048575`. Most servers return `206 Partial Content`.
2. Verify ar magic. Read `debian-binary` member header, skip its data.
3. Read second member header. Confirm name starts with `control.tar.`.
4. Note declared size `S` and offset `O` of its data within the file.
5. If `O + S <= len(buffer)`: slice it out, decompress per the suffix,
   tar-walk for `./control`, parse with `pault.ag/go/debian`.
6. Else: issue `Range: bytes=O-(O+S-1)`, splice with what we already have.

Server compatibility:

- GitHub's release-asset URLs redirect to S3-backed object storage that
  honors `Range` consistently. Confirmed in production by countless apt
  mirrors that depend on it.
- If a server returns `200` instead of `206` (no Range support), we drain
  the full body. We can still extract control from the prefix as we read,
  and use the stream as our SHA256 path. This is the same code path as
  the "digest missing" fallback, so it costs nothing extra.

## GitHub discovery

Per source:

```yaml
sources:
  vendor-bar-amd64:
    discovery:
      type: github_release
      repo: bar-org/bar
      asset: 'bar_.*_amd64\.deb'
      include_prerelease: false
      # token_env:  GITHUB_TOKEN_PRIVATE         # optional override
      # token_file: /run/secrets/private.token   # mutually exclusive with token_env
```

- Endpoint: `GET /repos/{repo}/releases/latest` (or list+filter when
  `include_prerelease: true`).
- Send `If-None-Match: <cached etag>`; cache the response `ETag` in state.
- **Auth precedence** (highest first): per-source token
  (`discovery.token_env` or `discovery.token_file`) → global token
  (`github.token_env` or `github.token_file`) → unauthenticated. At
  each level `_env` and `_file` are mutually exclusive. Realistic
  deployment shape: one global token via
  `github.token_file: /run/secrets/github.token` for production, or
  `github.token_env: GITHUB_TOKEN` for development. Per-source
  overrides exist for the rare case of one source needing a different
  identity (e.g., a private repo under a different org).

### Token handling

Independent of delivery form:

- Read once at startup into a `[]byte`, not a Go `string` — strings are
  immutable and cannot be zeroed. Best-effort zero on shutdown.
- Never logged. Bucket-key traceability uses `sha256(token)[:16]`.
- Always sent as `Authorization: Bearer <token>`. Never as a URL
  parameter — URLs leak through proxies, `Referer`, server access logs,
  process listings.
- State files never persist the token or anything derived from it
  beyond the bucket-key hash.
- A configured token field that resolves to an empty value is a hard
  startup failure, not a silent fall-through to unauthenticated. Same
  strictness rule as everywhere else.

### Asset selection

- Regex is implicitly anchored to the full asset name (matched as
  `^<asset>$`). **Exactly one** match required. Zero or multiple
  matches → source-level error: logged at ERROR, the source holds at
  prior state, other sources continue. No silent fall-back to "first
  wins" or "skip and try next time" — a vendor renaming their assets
  must surface as a loud failure.
- One source = one asset. To publish both amd64 and arm64 from a
  single upstream release, declare two sources with different regexes.

## GitHub rate limiting

GitHub's REST API limits per credential:

- Unauthenticated: 60 requests/hour per source IP.
- Authenticated (PAT or OAuth token): 5000 requests/hour per token.

Conditional requests via `If-None-Match` returning 304 **still count**
against the budget, so ETag caching saves bandwidth but not quota.

The signpost defends both predictively (refuse calls we know would
exceed budget) and reactively (honor GitHub's response headers when
they push back).

### Predictive: token bucket per credential

```yaml
github:
  rate_limit:
    unauthenticated_per_hour: 50    # default; GitHub allows 60, headroom for safety
    authenticated_per_hour: 4500    # default; GitHub allows 5000, headroom for safety
```

A token bucket per credential, replenished continuously at the
configured rate. Bucket capacity equals one hour's worth of tokens.
Bucket key is computed against the **effective** token for the source
(per-source override, else global default, else none):

- Effective token resolves to a non-empty string: bucket key is
  `sha256(token)[:16]`. We never log or store the plain token. Sources
  resolving to the same token share a bucket — the natural unit, since
  GitHub itself buckets per credential. With a single global token
  (`github.token_env` or `github.token_file`), every authenticated
  source shares one 4500-req/h budget.
- Effective token is empty: a single shared `unauthenticated` bucket.
  GitHub buckets these by IP, so all our unauth calls hit the same
  GitHub-side budget anyway.

If a source's tick wants a token and the bucket is empty, the tick is
skipped for that source (logged) and retried on the next refresh
interval. No queueing, no global pause.

### Reactive: honor GitHub's response headers

Every GitHub API response carries `X-RateLimit-Remaining` and
`X-RateLimit-Reset`. After each response:

- If `Remaining == 0`, or the response was 403 with a `Retry-After`
  header or a rate-limit-exceeded body: mark that credential's bucket
  exhausted until `Reset` (or `now + Retry-After`). Subsequent ticks
  for sources on that bucket skip without issuing an HTTP call.
- Otherwise: nothing extra. The predictive throttle is doing the work.

Hard-coded behavior, no config knob. Catches secondary limits and any
case where GitHub tightens thresholds without warning.

## Configuration (MVP shape)

```yaml
repository:
  origin: "Acme"
  label: "Acme APT"
  base_url: "https://apt.acme.example"

suite:
  codename: "stable"
  description: "Acme stable channel"
  architectures: ["amd64", "arm64"]
  # component is always "main" in MVP
  # suite name is always "stable" in MVP

bootstrap:
  package_name: "acme-archive-keyring"
  maintainer: "Acme Ops <ops@acme.example>"
  description: |
    Acme APT repository signing key and sources list.
  # File layout is generated, not user-templated:
  #   /usr/share/keyrings/<package_name>.gpg
  #   /etc/apt/sources.list.d/<package_name>.sources
  # Versioning is YYYY.MM.DD.N, auto-bumped on input change.

signing:
  key_file: "/var/lib/signpost/secring.gpg"
  # Pick at most one passphrase source. Mutually exclusive.
  passphrase_file: "/run/secrets/signpost.gpg.passphrase"   # preferred for production
  # passphrase_env: STITCHER_GPG_PASSPHRASE                 # dev / quick start
  # next_pubkey_file: "/var/lib/signpost/next.pub"          # optional; see Key rotation

server:
  listen: ":8080"

refresh:
  interval: "1h"
  jitter: "5m"
  http_timeout: "60s"

github:
  # Default credential for all github_release sources.
  # token_env and token_file are mutually exclusive at this scope.
  token_file: /run/secrets/github.token   # preferred for production
  # token_env: GITHUB_TOKEN               # dev / quick start
  rate_limit:
    unauthenticated_per_hour: 50      # safety headroom under GitHub's 60
    authenticated_per_hour: 4500      # safety headroom under GitHub's 5000

paths:
  state_dir: "/var/lib/signpost/state"

sources:
  vendor-bar-amd64:
    discovery:
      type: github_release
      repo: bar-org/bar
      asset: 'bar_.*_amd64\.deb'
      # token_env: GITHUB_TOKEN_PRIVATE  # override only when this source
      #                                  # needs a different identity
```

Notes vs. the full design:

- `suites:` (map) collapses to `suite:` (single object). The suite
  **name** (the `dists/<name>/` URL segment) is hardcoded `stable` in
  MVP. `suite.codename` is the value emitted in the `Release` file's
  `Codename:` field — typically the same string but allowed to diverge
  (e.g., codename `bookworm`).
- No `component` per source — always `main`.
- Discovery accepts only `type: github_release`.
- `${VAR}` env interpolation in string values, same rules as full design.

## Validation rules (startup, hard-fail)

- `repository.base_url` is `http(s)://...`, no path, no trailing slash.
- Each `sources:` map key matches `^[a-z0-9][a-z0-9._-]*$`. Uniqueness is
  enforced by the YAML parser (strict mode rejects duplicate keys).
- `discovery.type` is `github_release`.
- `discovery.repo` matches `^[^/]+/[^/]+$`.
- `discovery.asset` compiles as a Go regex.
- `signing.key_file` exists, mode `0400`, owned by service user.
- `signing.next_pubkey_file`, if set, exists and parses as an OpenPGP
  public key. Distinct fingerprint from `key_file`.
- `signing.passphrase_env` and `signing.passphrase_file` are mutually
  exclusive. If `passphrase_file` is set: exists, mode `0400`, owned
  by service user, contains a non-empty value (whitespace-trimmed).
  If `passphrase_env` is set: env var resolves to a non-empty string.
  Empty value at either form is a hard startup failure.
- `bootstrap.package_name` is a valid Debian package name
  (`^[a-z0-9][a-z0-9+\-.]+$`).
- `bootstrap.maintainer` and `bootstrap.description` are non-empty.
- `github.token_env` and `github.token_file` are mutually exclusive.
  Same rule for per-source `discovery.token_env` vs
  `discovery.token_file`.
- `github.token_env` (if set) names an env var that resolves to a
  non-empty string. Empty value is a hard startup failure.
- `github.token_file` (if set) names a path that exists, is mode
  `0400`, owned by the service user, and contains a non-empty value
  (whitespace-trimmed). Same rule for per-source `discovery.token_file`.
- `github.rate_limit.unauthenticated_per_hour` and
  `github.rate_limit.authenticated_per_hour` are positive integers
  (defaults applied if unset).
- All file paths in config (`signing.key_file`, `signing.next_pubkey_file`,
  `signing.passphrase_file`, `github.token_file`, per-source
  `discovery.token_file`, `paths.state_dir`) must be absolute.
- All durations parse via `time.ParseDuration`.

## CLI

```
signpost serve  [--config FILE]
signpost check  [--config FILE] [--source ID]
```

- `serve` runs the daemon: validate config, synchronously import any
  new sources, sign initial snapshot, start HTTP, schedule the refresh
  ticker.
- `check` is read-only pre-flight validation. Same pipeline as a
  refresh tick (config validate → discovery probe → range-fetch +
  control parse → SHA256 verify), but writes a per-source report to
  stdout and exits non-zero on any failure. Doesn't write state files,
  doesn't talk to a running daemon — safe to run in CI or on a
  workstation against a candidate config. `--source ID` scopes to a
  single source.
- **Caveat**: `check` makes its own GitHub API calls and does **not**
  share the running daemon's rate-limit buckets — both processes draw
  from the same GitHub-side budget. Run `check` outside busy refresh
  windows when a daemon is live with the same credential.

Sample output:

```
ok    vendor-bar-amd64    bar 1.2.3 amd64    sha256=ok  range=206
ok    vendor-bar-arm64    bar 1.2.3 arm64    sha256=ok  range=206
FAIL  vendor-foo-amd64    asset regex matched 0 assets in v2.0.0
exit 1
```

## Adding (or removing) a tracked package

Workflow for adding:

1. Add a new key under `sources:` in `config.yaml` (matching
   `^[a-z0-9][a-z0-9._-]*$`) with a `discovery: { type: github_release, ... }`
   block.
2. Run `signpost check --config <path>` (optionally
   `--source <new-name>`). Iterate on the config until it reports `ok`.
3. Restart the daemon. Startup synchronously imports any source that
   lacks a state file; the new package is in `Packages` from the very
   first request after HTTP comes up. If the new source still fails at
   that moment (upstream went down between `check` and restart), the
   daemon exits loudly rather than serving a half-broken view.

Removing a package:

- Delete the entry from `config.yaml`, restart. The orphaned
  `state/sources/<name>.json` is harmless and ignored — clean it up by
  hand if it bothers you. MVP has no `signpost prune`.

Renaming a source:

- Treated as remove + add. Old state file is orphaned; the new key is
  synchronously imported on the next restart.

Forcing a fresh import on a source whose discovery config changed:

- Delete `state/sources/<name>.json` before restart. The daemon will
  treat it as new and run the synchronous import path.

## Suggested package layout

```
cmd/signpost/         main, config wiring
internal/config/      YAML schema, validation, env interpolation
internal/source/      Discoverer interface + github_release impl
internal/fetch/       range-fetch, ar parsing, control extraction
internal/refresh/     tick loop, change detection
internal/index/       Packages, Release writers (SHA256-only)
internal/sign/        openpgp wrapper
internal/bootstrap/   nfpm-driven keyring/.sources package builder
internal/store/       per-source JSON state read/write
internal/server/      http: static files, by-hash, pool redirects,
                            /release/..., /pubkey.gpg
```

## Key rotation

apt verifies `InRelease` against the local file at
`Signed-By: /usr/share/keyrings/<pkg>.gpg`. Rotation has a chicken-and-
egg problem: a client whose local keyring doesn't contain the new
signing key can't verify the bootstrap `.deb` that would *install* that
new key. The fix is to **pre-position** the next pubkey in the bootstrap
*before* switching the active signer.

The mechanism is one optional config field:

```yaml
signing:
  key_file: /var/lib/signpost/current.sec
  next_pubkey_file: /var/lib/signpost/next.pub   # optional
```

`next_pubkey_file` is **only** a packaging input — the signpost never
signs with it, only embeds it in the bootstrap `.deb`'s keyring. The
secret half of the next key can stay in cold storage through the entire
pre-positioning phase.

### Rotation phases

1. **Steady state.** No `next_pubkey_file`. Bootstrap ships
   `{current.pub}`. `InRelease` signed by `current.sec`. Clients trust
   `{current.pub}`.

2. **Pre-position (soak).** Operator generates new keypair offline,
   drops `next.pub` on the signpost host, adds `next_pubkey_file` to
   config. On the next refresh tick the bootstrap input hash changes
   (pubkey set went from `{current}` to `{current, next}`), the `.deb`
   rebuilds and version-bumps, and `Packages` references the new
   bootstrap. `InRelease` is **still** signed by `current.sec` — clients
   still trust it, so `apt upgrade` cleanly pulls the new keyring into
   every fleet member's local file. Operator waits long enough for ~all
   clients to update — a week is comfortable, longer for laggy fleets.

3. **Cutover.** Operator deploys `next.sec` to the signpost host, flips
   config so `key_file` points at it (and clears `next_pubkey_file`,
   or sets it to the *previous* pubkey for one more cycle as defense in
   depth). Next refresh tick rebuilds the bootstrap with `{next.pub}`
   and signs `InRelease` with `next.sec`. Clients who completed phase 2
   verify fine. Clients who skipped phase 2 are locked out and need
   manual recovery (`curl /pubkey.gpg | sudo tee
   /etc/apt/keyrings/<pkg>.gpg`).

The soak length in phase 2 is the operator's only lever against the
locked-out failure mode. Rotate rarely, give long soaks, communicate
the schedule.

## Atomicity & failure isolation

Identical to the full design:

- Per-source state files written tmp + `rename`.
- Snapshot publish is one `atomic.Pointer.Store`.
- Source-level failures hold at last-known-good; tick continues.
- Sign/build failures retain the prior snapshot.
- Process crash mid-tick rebuilds last-known-good from disk on restart.

## Testing

- **Unit**: ar header parser (round-trip on canned `.deb` headers),
  range-fetch logic (mock HTTP server returning 200/206/missing
  headers), control extraction (golden-file across gz/xz/zst control
  archives), index writers (golden `Packages` and `Release` outputs),
  token-bucket math.
- **Integration**: against real public GitHub releases. Two fixtures
  cover both architecture code paths:
  - `goreleaser/nfpm` — ships arch-specific `.deb`s
    (`nfpm_*_linux_amd64.deb`, etc.). Exercises the
    `Architecture: <arch>` path: parsed arch must match one of
    `suite.architectures` and the stanza lands only in that arch's
    `Packages`.
  - `vkbo/novelwriter` — ships an `Architecture: all` `.deb`.
    Exercises the `all`-arch fan-out: one parsed stanza must appear
    in every configured `binary-<arch>/Packages`.

  Both also stress the range-fetch + control-extraction path against
  GitHub's S3-backed asset storage. Tests run unauthenticated by
  default (public repos) and authed when `GITHUB_TOKEN`-style env is
  set; skipped only when network is unavailable, not when a token is
  absent.
- **End-to-end**: docker-compose harness — signpost fronts a fixed set
  of GitHub releases, an apt-client container does
  `apt update && apt install <pkg>`, exits non-zero if anything fails.
  Validates the trust chain end-to-end.
- **Crash recovery**: `kill -9` mid-tick, restart, assert the served
  snapshot matches last-known-good state files.

## What the MVP intentionally omits

- **MD5Sum / SHA1 in metadata** — not emitted, not consumed. Modern apt
  is fine; a pre-Stretch client would fail and that's accepted.
- **xz `Packages.xz`** — gzip only. Smaller code path, identical
  client-side behavior in practice.
- **History log** — `state/sources/<id>.history.jsonl` not written.
- **`Valid-Until`** — omitted from `Release`, same as full design.
- **CLI beyond `serve` and `check`** — no `force-refresh`, no
  `show-state`, no live config reload. Restart the process for any
  config change.
- **Metrics / `/status`** — MVP is logs-only (slog JSON to stdout).
  No Prometheus, no readiness probes, no structured status endpoint.
  Defer to v1.

## Open questions

None. All design points are decided for MVP.
