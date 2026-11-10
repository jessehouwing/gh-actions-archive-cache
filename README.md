# gh-actions-archive-cache

A compiled GitHub CLI extension for populating and maintaining the GitHub Actions
runner's repository-archive cache on Windows, Linux, and macOS.

For VM/container provisioning examples and an image acceptance checklist, see
[Prepopulate custom runner images](docs/custom-runner-images.md). Baking your
common actions into an image reduces repeated archive transfers across fresh
runner instances without disabling normal cache-miss downloads.

## Install

Requires Go as declared in `go.mod` for source builds, and an authenticated
[GitHub CLI](https://cli.github.com/). Precompiled releases do not require Go.

Until a precompiled release is available, clone this repository and build locally:

```powershell
go build -o gh-actions-archive-cache.exe ./cmd/gh-actions-archive-cache
.\gh-actions-archive-cache.exe --help
gh extension install .
```

On Linux/macOS, omit `.exe` and invoke `./gh-actions-archive-cache`. Local
installation requires a Git repository named `gh-actions-archive-cache`.
Once release binaries are available, install it with
`gh extension install jessehouwing/gh-actions-archive-cache`.

## Authentication and environment tokens

The extension detects credentials at runtime using the selected home host.
For a `github.com` home host, standard GitHub CLI discovery checks `GH_TOKEN`,
then `GITHUB_TOKEN`, then saved github.com authentication. Public repositories
can also be accessed anonymously when no token is available, with lower rate
limits. A token must have access to any private repositories being downloaded.

| Selected home host | Credentials for github.com requests, in priority order |
|---|---|
| `github.com` | `GH_TOKEN`, `GITHUB_TOKEN`, saved github.com authentication, anonymous |
| `TENANT.ghe.com` | `GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN`, saved github.com authentication, anonymous |

`GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN` is a separate override **only for
github.com requests during a tenant invocation**. It is not used when the home
host is github.com. During tenant invocations, `GH_TOKEN`/`GITHUB_TOKEN` are
reserved for tenant requests and are never forwarded to github.com, even if
they happen to contain a github.com token.

For example, have your secret manager inject a github.com token as `GH_TOKEN`
for a public-host run:

```powershell
# GH_TOKEN contains the github.com token supplied by your secret manager.
gh actions-archive-cache download actions/checkout@v4 --hostname github.com
```

For a tenant run that also needs github.com dependencies, inject the tenant
token as `GH_TOKEN` and the github.com token as
`GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN`:

```powershell
# GH_TOKEN contains the tenant token.
# GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN contains the github.com token.
gh actions-archive-cache from-lockfile --hostname TENANT.ghe.com
```

Alternatively, authenticate with `gh auth login --hostname HOST` for each host
and leave the corresponding environment overrides unset. An environment token
takes precedence over a saved login. Token strings are not inspected to infer
their host, and authentication failures do not trigger retries with a different
credential. API credentials are removed on archive redirects to other hosts.
Do not bake tokens or saved authentication into runner images.

### Running in GitHub Actions

**Yes: the extension can use the job's built-in `GITHUB_TOKEN`** for the selected
host. Explicitly expose `${{ github.token }}` (equivalently,
`${{ secrets.GITHUB_TOKEN }}`) as `GH_TOKEN` or `GITHUB_TOKEN` in the step's
environment. The token context is not automatically a shell environment
variable, and the extension does not obtain the runner's internal action-download
credentials.

For example, on github.com, after installing the extension and checking out a
repository containing `.github/workflows/actions.lock`:

```yaml
permissions:
  contents: read

steps:
  # Earlier steps install the extension and check out the repository.
  - name: Populate action archives
    shell: pwsh
    env:
      GH_TOKEN: ${{ github.token }}
    run: |
      $cache = Join-Path $env:RUNNER_TEMP 'actionarchivecache'
      gh actions-archive-cache from-lockfile --hostname github.com --cache-dir $cache
      if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
```

`contents: read` is sufficient for repository metadata and archives the token
is authorized to read. The job token is scoped to the workflow repository:
it can read public repositories on that host, but it does not grant general
access to other private action repositories. For those, use a GitHub App
installation token or a personal access token with access to the required
repositories. The runner's ability to download a shared private action does
not imply the job's `GITHUB_TOKEN` has the same access.

For a GHE.com workflow, set `--hostname TENANT.ghe.com` and use the job token as
`GH_TOKEN` for tenant requests. To authenticate github.com downloads during that
invocation, supply a **separate github.com credential** through
`GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN`; the tenant job token is not reused
publicly. Without that credential or saved github.com authentication, public
github.com requests are anonymous.

Running a preload step does **not** accelerate action preparation for that same
job: the runner prepares repository actions before executing steps. Use it to
build a custom runner image or prepare a persistent cache for subsequent jobs.
The temporary directory in the example is only a staging location, not a cache
that automatically survives the job; copy it into the image or another intended
persistent location. Do not retain the job token: it expires when the job ends
or reaches its maximum lifetime.

## Download versions

```shell
gh actions-archive-cache download actions/checkout --latest-major-versions 2 --latest-versions-per-major 5
gh actions-archive-cache download actions/cache/save@v4 --format both
gh actions-archive-cache download actions/checkout@main --dry-run
```

The version rules match `actions/action-versions`'s numeric filter:

- Stable `vMAJOR`, `vMAJOR.MINOR`, and `vMAJOR.MINOR.PATCH` tags only.
- X newest distinct majors; Y newest **individual version tags** per major,
  including patch releases and minor-only aliases, not Y distinct minor series.
- Major-only pointers such as `v4` are retained separately from the Y-tag limit.
- Both limits default to `0` (unlimited); negative limits are invalid.
- Branches and arbitrary tags are included only when requested explicitly with
  `@REF`. Exact refs and version-limit flags cannot be combined.

Selected refs are resolved to immutable commit SHAs, including annotated tags.
Aliases sharing a SHA download only one archive per requested format. An action
subpath shares its full repository archive with other actions in that repository.

`--format auto` is the default: ZIP on Windows, tar.gz on Linux/macOS. Use `zip`,
`tar.gz`, or `both` to provision other operating systems. Existing archives are
checked and validated locally; valid files are never downloaded again. Corrupt
or nonregular destinations produce an error, not a silent overwrite. In `both`
mode only missing formats are downloaded.

### Output and progress

Downloads and lockfile imports print the cache directory once, followed by
numbered results with the action ref, archive format, and source host:

```text
Cache: D:\test
[1/2] Cached         actions/checkout@v4 (zip, github.com)
[2/2] Downloaded     Azure/login@v2 (zip, github.com)

Done: 1 downloaded, 1 cached, 0 planned, 0 failed
```

Commit-only refs use a short SHA in human output. Progress messages on stderr
announce ref resolution, processing, and each archive download before it starts.
Counts refer to unique SHA/format pairs, not tag aliases. Downloads and local
archive validation currently run **sequentially**, not in parallel. There is
no byte-level progress bar.

Use `--json` on `download` or `from-lockfile` for automation:

```powershell
gh actions-archive-cache from-lockfile --json > result.json
```

Stdout contains one JSON document with `cache_directory`, `dry_run`, `results`,
`downloaded`, `cached`, `planned`, `failed`, and `errors`. Each result includes
`host`, `repository`, `ref`, full `sha`, `format`, `path`, and `status`
(`cached`, `downloaded`, `planned`, or `failed`), plus `error` on failure.
Progress is suppressed; diagnostics remain on stderr and failures return a
nonzero exit code. Argument, lockfile, resolution, or cache-opening errors that
occur before archive processing report only stderr and may produce no JSON.
The option is not supported on `configure` or `cleanup`.

## Lockfiles

```shell
gh actions-archive-cache from-lockfile
gh actions-archive-cache from-lockfile path/to/actions.lock --format both --dry-run
```

The default path is `.github/workflows/actions.lock`, generated by
[`github/gh-actions-lock`](https://github.com/github/gh-actions-lock).
The shared official parser handles supported v0.0.1/v0.0.2/v0.0.3 schemas.
Every recorded direct and transitive repository dependency is downloaded using
its pinned SHA, without moving or re-resolving refs. Currently `sha1` commits
are supported; other digest algorithms fail explicitly.

If a pinned archive is already cached, its import makes **no network requests**.
Importing is not a provenance verification of the lockfile. Docker images,
tool downloads, workflow scanning, and recursive discovery of unrecorded
dependencies are outside this extension's scope.

## GitHub Enterprise Cloud with data residency

```shell
gh auth login --hostname TENANT.ghe.com
gh actions-archive-cache download actions/checkout --hostname TENANT.ghe.com
gh actions-archive-cache from-lockfile --hostname TENANT.ghe.com
```

The home host defaults to the SDK's `GH_HOST`/configured default, **not inferred
from the lockfile's directory or a Git remote**. Specify `--hostname` when needed.
Only github.com and canonical `TENANT.ghe.com` hosts are supported.

For **unlocked** inputs, check the tenant repository first. Only a
repository-level 404 permits fallback to a **public** github.com repository.
Tenant repositories take precedence over namesakes on github.com. A missing ref,
authentication/authorization failure, rate limit, or network error does not
permit fallback. GitHub may mask inaccessible private repositories as 404;
check tenant permissions if a repository unexpectedly resolves publicly.

**Lockfile entries never switch hosts**, even on 404. Explicit hostnames are
authoritative; omitted v0.0.3 hostnames bind to the home host. Legacy dotcom
lockfiles retain their github.com binding. Foreign tenants and a repository
locked to multiple hosts are rejected.

See [Authentication and environment tokens](#authentication-and-environment-tokens)
for credential precedence and supplying separate tenant/github.com tokens.
Keep `GH_TOKEN` appropriate for the selected home host; a wrongly scoped
environment token takes precedence over a valid saved login.

Archives are downloaded directly from their selected source host, including
github.com for github.com-bound dependencies, rather than through a tenant proxy.

## Configure a runner, user, or system

```shell
gh actions-archive-cache configure --scope runner --runner-path RUNNER_DIRECTORY --cache-dir CACHE_DIRECTORY
gh actions-archive-cache configure --scope user --dry-run
gh actions-archive-cache configure --scope system --cache-dir CACHE_DIRECTORY
```

Downloads do not implicitly modify persistent settings. Configuration sets
`ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE` and preserves unrelated settings.

| Scope | Windows | Linux | macOS |
|---|---|---|---|
| Runner | Selected runner's `.env` | Selected runner's `.env` | Selected runner's `.env` |
| User | User environment registry | `.profile`, `.bashrc`, `.zprofile`, `.zshrc` exports | `.bash_profile`, `.bashrc`, `.zprofile`, `.zshrc` exports |
| System | Machine environment registry | `/etc/environment` | `/etc/profile`, `/etc/bashrc`, `/etc/zprofile`, `/etc/zshrc` exports |

The Windows machine setting, Linux `/etc/environment`, and macOS shell-export
model follow `actions/runner-images`. Unix user exports support sh/bash/zsh
startup; unsupported user shells require explicit configuration or runner scope.
macOS has no universal persistent environment for every launchd service.
Use **runner scope for services**; the runner loads `.env` on startup on all
three platforms. `--runner-path` must contain `bin/Runner.Listener` (`.exe` on
Windows).

Restart affected runners; reopen shells or log in again. Existing processes
and the parent shell are not updated by this CLI. No automatic elevation,
runner restart, service reconfiguration, or global permission relaxation occurs.
System settings need the appropriate privileges. If a multi-file configuration
fails, earlier reported files may already have been updated; fix permissions and
rerun idempotently.

Cache-directory precedence: `--cache-dir`, then the environment variable, then
`$HOME/actionarchivecache` for user/runner operations. System defaults are
`C:\actionarchivecache` on Windows, `/opt/actionarchivecache` on Linux, and
`$HOME/actionarchivecache` on macOS. Paths are made absolute. Ensure the actual
runner account has read access; newly downloaded archives are not world-writable.

## Clean older versions

```shell
gh actions-archive-cache cleanup actions/checkout --latest-major-versions 2 --latest-versions-per-major 5 --format both --dry-run
gh actions-archive-cache cleanup actions/checkout --latest-major-versions 2 --latest-versions-per-major 5 --format both
gh actions-archive-cache cleanup actions/checkout --latest-major-versions 1 --lockfile other/actions.lock
```

Cleanup uses the same retention rules and requires at least one positive limit.
**Without `--dry-run`, it deletes identified older archive files.** There is no
additional `--apply` flag, and downloads never trigger cleanup automatically.

Cleanup preserves retained tags' shared SHAs, explicit exact-ref downloads,
previously imported pins, supplied `--lockfile` pins, and the current directory's
default lockfile when present. Pin/exact-ref protection recorded in the metadata
is conservative and persists across invocations; there is no automatic pin expiry.
Unseen lockfiles elsewhere cannot be discovered globally.

Only the selected repository and formats are eligible. SHA filenames do not
encode versions: a versioned metadata index plus current upstream tags establish
identity, including pre-existing archives where possible. Unidentified files
are **kept and warned about on stderr**, with their path and reason, in normal
and dry-run modes. Warnings alone do not fail cleanup. Summary counts include
unidentified files. Symlinks and directories are never followed or deleted.

Planning errors prevent all deletions. Individual deletion failures return a
nonzero exit and may leave a partially completed cleanup. The CLI locks real
cache operations; runners do not honor that lock, so clean only while affected
runners are idle. A crashed CLI may leave `.gh-actions-archive-cache.lock`;
verify no operation is active before manually removing that specific file.

## Cache contract and limits

The runner looks for:

```text
CACHE/owner_repo/COMMIT_SHA.zip
CACHE/owner_repo/COMMIT_SHA.tar.gz
```

GitHub's enclosing repository directory is preserved. Downloads are streamed to
owned temporary files, validated, and atomically published. Archives are not
extracted or symlinked into action work directories. Download and expanded
validation limits are 2 GiB. GitHub authentication/ref resolution still occurs
inside the runner; the archive cache does not make an entire job offline.
Missing entries retain the runner's normal network fallback.

Repository directory names preserve GitHub's canonical casing, for example
`Azure_login`, because runner lookups are case-sensitive on Linux. Lockfile keys
remain case-insensitive identities. Existing pinned archives can be found
locally without a metadata request; missing pinned entries use their bound
host's repository metadata to determine the directory name. Repository renames
or transfers require using the current repository name explicitly.

The runner layout has no host component, so the extension refuses to mix known
same-named repositories from different hosts in one cache. The metadata index
is `.gh-actions-archive-cache.json`; keep it to retain historical identity and
pin protection. This cache should be writable only by trusted administrators.

## Development and tests

Go quality gates match `github/gh-actions-lock`:

```shell
make fmt-check vet test
make test-unit
make test-integration
```

Native PowerShell equivalents, without Make or Bash:

```powershell
.\script\check.ps1 All
.\script\check.ps1 Unit
.\script\check.ps1 Integration
```

The underlying commands are `gofmt -l .` (explicitly fail if any paths are
returned), `go vet ./...`, `go test -race -count=1 ./internal/... ./cmd/...`,
and `go test -race -count=1 ./test/integration/...`. Aggregate
`go test -race -count=1 ./...` runs both suites. Race tests need `CGO_ENABLED=1`
and an appropriate C compiler (MinGW/LLVM-MinGW on Windows).

Unit tests cover internal rules and command wiring. Integration tests compile
and execute the actual CLI entrypoint with the `integrationtest` build tag,
which injects loopback-only HTTP fixtures and isolated configuration backends.
Those hooks are absent from production/release binaries. Tests require no live
tokens and never modify the machine's actual persistent user/system environment.
CI has separately visible blocking native unit/integration jobs on all three
OSes, plus formatting/vet and six-platform cross-build checks.

Tagged releases run quality/tests before `cli/gh-extension-precompile` publishes
amd64/arm64 binaries for Windows, Linux, and macOS with provenance attestations.
No release is created merely by building locally.

## License

[MIT](LICENSE)

## References

- [Runner archive lookup](https://github.com/actions/runner/blob/main/src/Runner.Worker/ActionManager.cs)
- [First-party cache builder](https://github.com/actions/action-versions)
- [Runner-image provisioning](https://github.com/actions/runner-images)
- [Dependency lockfiles](https://github.com/github/gh-actions-lock)
