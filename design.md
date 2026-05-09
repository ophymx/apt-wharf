# apt-signpost — Design

## Problem

Several Debian packages we depend on are distributed as standalone `.deb`
downloads — the upstream vendors don't publish apt repositories. Today this is
patched over with hand-rolled scrapers feeding a `reprepro` repo, which means
we end up storing and re-serving bytes we don't need to host.

## Approach

Stand up an apt repository that:

- Hosts and signs the repository metadata (`InRelease`, `Release`,
  `Release.gpg`, `Packages`, `Packages.gz`, `Packages.xz`).
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
period — installed clients already trust the next key.

## URL layout

```
/dists/stable/InRelease
/dists/stable/Release[.gpg]
/dists/stable/main/binary-<arch>/Packages[.gz|.xz]

/pool/main/<...>/<name>_<version>_<arch>.deb       # external → 302 redirect
/pool/main/<...>/<repo>-archive-keyring_<v>_all.deb # internal → real file

# Bootstrap convenience endpoints
/<repo>-archive-keyring.gpg                         # raw public key
/release/<suite>/<arch>/latest.deb                  # 302 → versioned bootstrap .deb
/release/<suite>/<arch>/latest                      # plain text version string
```

Suite support starts at just `stable`, but the `dists/<suite>/` layout is
preserved so adding more is non-structural.

## Implementation stack

Pure Go, no CGO.

- **`github.com/goreleaser/nfpm/v2`** — building the bootstrap `.deb`.
- **`pault.ag/go/debian`** — reading upstream `.deb` files and parsing
  control stanzas (saves rolling our own ar+tar+control parser).
- **`github.com/ProtonMail/go-crypto/openpgp`** — in-process clearsign for
  `InRelease` and detached sign for `Release.gpg`.
- **`compress/gzip`** + **`github.com/ulikunitz/xz`** — `Packages.gz`,
  `Packages.xz`.
- **No database.** Per-source JSON state files on disk; the served
  repository is held in memory and published by an `atomic.Pointer` swap.

## Persistent state

```
state/
  sources/<source-id>.json           # latest known state per source
  sources/<source-id>.history.jsonl  # optional append-only log of changes
  bootstrap.json                     # input-hash + current version
  bootstrap/<version>.deb            # cached bootstrap .deb bytes
```

Per-source state captures: discovered URL, version, hashes (sha256/sha1/md5),
size, full control stanza for `Packages`, last-checked / last-changed
timestamps.

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
       inputs = base_url ⊕ bootstrap.* ⊕ suites ⊕ pubkey-bytes
       if hash(inputs) == state/bootstrap.json.input_hash: reuse cached .deb
       else: nfpm-build, bump version, persist state/bootstrap.json + .deb.
  4. Build new snapshot:
       per-source stanzas → Packages → gzip/xz
       per-suite Release referencing those file hashes
       sign → InRelease (clearsigned) + Release.gpg (detached)
       compose files map + redirects map.
  5. current.Store(newSnapshot).
  6. Append history entries for changed sources (best-effort).
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
- Optional `GITHUB_TOKEN` env for higher rate limits.

### Built-in: `external`

Escape hatch for vendors with bespoke discovery needs. Language-agnostic
contract over stdio.

```yaml
discovery:
  type: external
  command: ["/usr/local/libexec/discover-baz", "--channel", "stable"]
  timeout: 30s
```

- Stitcher executes `command` with a clean environment (allowlist:
  `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, plus per-source `env` config).
- **stdin**: one JSON object — `{"prev": {"url": "...", "token": "..."}}`
  (`prev` omitted on cold start).
- **stdout**: one JSON object — either `{"url": "...", "token": "..."}` or
  `{"unchanged": true}`.
- **stderr**: free-form, captured into signpost logs.
- **exit code**: 0 success, non-zero error (source holds at previous state,
  error logged).
- Hard kill on timeout.

External commands return discovery metadata only. They do **not** download
the `.deb` themselves — the refresher owns all HTTP, hashing, and error
handling. If a vendor needs auth headers to fetch, that goes in a separate
fetch-options config, not in the script.

## GPG / signing

Configurable, but always non-interactive at runtime. Two supported modes:

- **Unencrypted key file** — config points at the secret key file; tool
  loads it on startup. Startup checks file mode `0400` and ownership.
- **Passphrase-protected key file** — passphrase from env var or a file
  (mode `0400`). Loaded once at startup, key kept unlocked in memory for the
  lifetime of the process.

Signing is in-process via `github.com/ProtonMail/go-crypto/openpgp`. No
external `gpg` binary, no `gpg-agent`. Smartcard / hardware-key support is
explicitly out of scope.

## Suggested package layout

```
cmd/signpost/         main, flag/config wiring
internal/config/      YAML schema, validation
internal/source/      Discoverer interface + built-in impls
internal/refresh/     poll loop, change detection, control extraction
internal/index/       Packages, Release, InRelease writers
internal/sign/        openpgp wrapper (key load, clearsign, detach)
internal/bootstrap/   nfpm-driven keyring/.sources package builder
internal/store/       per-source JSON state read/write
internal/server/      http: static metadata, redirector, /release/... endpoints
```

## Configuration

YAML, loaded once at startup. A complete annotated example lives at
[`config.example.yaml`](config.example.yaml); this section covers the
schema semantics that aren't obvious from the example.

### Top-level shape

```
repository:    # Origin/Label/base_url for the published Release file and bootstrap .sources
suites:        # map of suite name → { codename, description, components, architectures }
bootstrap:     # bootstrap .deb metadata (package_name, maintainer, description)
signing:       # key_file + at most one of passphrase_env | passphrase_file
server:        # listen address, access log mode
refresh:       # interval, jitter, http_timeout
paths:         # state_dir
sources:       # map of source name → per-source config (see below)
```

### Per-source shape

```
sources:
  <source-name>:                   # key matches ^[a-z0-9][a-z0-9._-]*$
    enabled: true                  # default
    suite: stable                  # default; must exist in suites:
    component: main                # default; must be in that suite's components
    discovery: { type: ..., ... }  # required, discriminated union (below)
    fetch:                         # optional
      headers: { ... }             # sent on the .deb fetch
```

Sources are a map keyed by name (not a list with an `id` field) — the YAML
parser rejects duplicates in strict mode, and the structure mirrors `suites:`.
The Debian package name, version, architecture, and full control stanza all
come from introspecting the downloaded `.deb` — they are not declared in
config. The map key is purely an internal handle for state filenames and pool
paths.

### Discovery union

`discovery.type` is the discriminator; the other keys under `discovery` are
validated against the matching variant. In Go terms:

```go
type Discovery interface{ Type() string }

type LatestURL struct { URL string }
type GitHubRel struct {
    Repo string; Asset string         // asset is a Go regex over asset names
    IncludePrerelease bool
    TokenEnv string                   // optional
}
type External  struct {
    Command []string
    Timeout time.Duration
    Env     map[string]string         // explicit allowlist passed to the child
}
```

A wrapper type implements `UnmarshalYAML`, peeks at `type`, and decodes into
the right concrete struct. Unknown `type` values fail config validation at
startup.

### Env var interpolation

Any string value supports `${VAR}` expansion at config load. Required for
tokens that should not sit in plaintext config (`GITHUB_TOKEN`, vendor
download tokens, external-command env). Missing vars fail loud at startup,
not silently to empty string.

Scope: applied during YAML decode for all `string` and `[]string` fields.
Not applied inside `discovery.external.command` argv values — commands can
do their own env expansion if they want, which keeps the contract clean.

### Validation rules (enforced at startup, hard-fail)

- `repository.base_url` is a valid `http`/`https` URL with no path or
  trailing slash.
- Each source map key matches `^[a-z0-9][a-z0-9._-]*$`. Uniqueness is
  enforced by the YAML parser (strict mode rejects duplicate keys).
- `suite` referenced by every source exists in `suites`.
- `component` referenced by every source is in that suite's `components`.
- `discovery.type` is one of the registered types.
- `signing.passphrase_env` and `signing.passphrase_file` are mutually
  exclusive.
- `signing.key_file` exists, mode `0400`, owned by the service user.
- All durations parse via `time.ParseDuration`.
- For `latest_url`: `url` is a valid http(s) URL.
- For `github_release`: `repo` matches `^[^/]+/[^/]+$`; `asset` compiles
  as a Go regex.
- For `external`: `command` non-empty, `command[0]` exists and is
  executable.

### Intentionally not configurable in v1

- Per-source refresh interval — global only.
- Output compression formats — always emit `Packages`, `Packages.gz`,
  `Packages.xz`.
- Hash algorithms in `Packages` — always SHA256 + SHA1 + MD5 + Size
  (apt still expects all three legacy hashes in places).
- Bootstrap file layout — generated, not user-templated.

## Open questions

- **Operational surface** — CLI for "force refresh source X", "show state",
  "rotate key"? `/status` HTTP endpoint for monitoring? Not yet decided.
- **Bootstrap keyring contents during rotation** — confirm "current + next"
  is the policy (vs. "current only" or "current + previous").
- **`Architecture: all` packaging** — bootstrap is `all`, so it should land
  in every `binary-<arch>/Packages`. Confirm vs. a separate `binary-all`
  listed only in `Components`.
- **Failure visibility** — how do persistent discovery failures surface?
  Log only, structured metric, or also reflected in a `/status` endpoint?
