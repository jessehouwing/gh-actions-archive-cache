# Prepopulate custom runner images

Bake frequently used action archives into a VM or container image so each fresh
self-hosted runner starts with them already available. This is especially useful
for autoscaling/ephemeral runners behind a shared outbound gateway.

The runner still resolves action references with GitHub at job startup. A cache
hit avoids transferring and downloading the **repository archive**, not all
GitHub API traffic, runner registration, container pulls, or tool downloads.

## What goes into the image

```text
Image build
  install the runner and gh-actions-archive-cache
  download common action versions or lockfile dependencies
  set ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE for the runner
  remove build credentials and publish the image

Runner startup
  register the runner using fresh runtime credentials
  start the runner with the baked-in cache setting
  use cached archives when resolved SHAs match
  download normally when a requested archive is missing
```

Archives are keyed by **commit SHA**, not a tag label. A moving tag such as `v4`
can point to a different commit after an image is built. Keep rebuilding the
image; the runner safely downloads missing commits rather than using an archive
from an old tag resolution.

ZIP is used by Windows runners; tar.gz is used by Linux and macOS. Archive
contents are architecture-independent, but the extension binary itself must
match the image builder's OS/architecture. Use `--format both` only when
preparing a cache to distribute to more than one operating system.

## Prerequisites

1. Install GitHub CLI and the runner binaries in the image build environment.
2. Install this extension once a precompiled release is available:
   `gh extension install jessehouwing/gh-actions-archive-cache --pin VERSION`.
   Replace `VERSION` with the actual release tag.
   Until then, build the binary from this source tree as described in the
   [README](../README.md) and invoke it directly instead of through `gh`.
3. Give the build process GitHub credentials using your image builder's secret
   mechanism. Use the minimum access needed to read the selected repositories.
4. Choose a cache path that will be present at runtime, and determine the
   runner installation path and runtime account.

Image-building tools such as Packer, Azure VM Image Builder, or a container build
can run the commands below in their provisioner steps. Do not register a runner
or save a runner registration token in a reusable image.

### Build-time token discovery

For commands with `--hostname github.com`, the extension automatically checks
`GH_TOKEN`, then `GITHUB_TOKEN`, then saved github.com authentication. Public
repositories can be downloaded anonymously if none is available, but a token
provides higher rate limits. Private repositories require appropriate access.
The examples below expect your build system to inject the token into the
provisioner process; no interactive login is needed when an environment token
is present.

For `--hostname TENANT.ghe.com`, use tenant credentials in `GH_TOKEN` (or
`GITHUB_TOKEN`). To authenticate github.com requests in that same invocation,
inject a separate token as `GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN`;
otherwise saved github.com authentication is tried, followed by anonymous
access. Tenant invocations never send `GH_TOKEN`/`GITHUB_TOKEN` to github.com.
The separate override is not consulted for a github.com home host.

Credentials are chosen by host configuration, not by inspecting the token.
A wrong environment token overrides a valid saved login; authentication errors
do not cause credential fallback. See the
[README authentication reference](../README.md#authentication-and-environment-tokens).
Never leave tokens or saved `gh` authentication in the final image.

### Image builds orchestrated by GitHub Actions

You can use the workflow job's built-in token instead of a separate secret when
its repository access is sufficient. Set `permissions: contents: read` and
explicitly pass `GH_TOKEN: ${{ github.token }}` in the provisioning step's
`env`. `${{ secrets.GITHUB_TOKEN }}` is equivalent. The extension discovers the
environment variable; it does not read the Actions token context directly or
borrow the runner's internal credentials. See the
[workflow example and access limits](../README.md#running-in-github-actions).

The job token can read public repositories on its host and its authorized
workflow repository, but not arbitrary other private action repositories.
Use a suitably scoped GitHub App installation token or personal access token
for those. For a tenant-hosted job, use its token for `TENANT.ghe.com` and inject
a separate github.com credential as
`GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN` if authenticated public-host requests
are needed.

Step environment variables do not automatically reach a remote Packer/VM image
provisioner or a Docker build. Forward the token through that builder's
ephemeral secret mechanism. For the BuildKit example below, an Actions step with
`GH_TOKEN` in its environment can pass
`docker build --secret id=github_token,env=GH_TOKEN .`.
Do not pass it as a build argument or save it in the resulting image. Job tokens
are short-lived; finish provisioning while the issuing job is still active.

This prepares **future runner instances/jobs**, not the image-building job's own
action startup: repository actions for that job are prepared before its first
step executes. A cache in the build runner's temporary directory is not
automatically included in the published image.

## Linux VM image example

These are build-time shell commands, not a workflow running after a job has
already started. Assume runner binaries are installed in `/opt/actions-runner`,
the extension is installed for the build account, and a token is available only
to the provisioner process.

```bash
set -euo pipefail

CACHE=/opt/actionarchivecache
RUNNER=/opt/actions-runner

gh actions-archive-cache download actions/checkout \
  --latest-major-versions 2 --latest-versions-per-major 3 \
  --cache-dir "$CACHE" --format tar.gz --hostname github.com

gh actions-archive-cache download actions/setup-node \
  --latest-major-versions 2 --latest-versions-per-major 3 \
  --cache-dir "$CACHE" --format tar.gz --hostname github.com

gh actions-archive-cache download actions/cache \
  --latest-major-versions 1 --latest-versions-per-major 3 \
  --cache-dir "$CACHE" --format tar.gz --hostname github.com

gh actions-archive-cache configure --scope runner \
  --runner-path "$RUNNER" --cache-dir "$CACHE"
```

Runner scope writes the selected installation's `.env`, which `Runner.Listener`
loads when it starts, including as a service. This avoids relying on whether
systemd inherited a shell profile or `/etc/environment`.

If configuring a general-purpose image before installing the runner, use
`configure --scope system --cache-dir /opt/actionarchivecache` to follow the
runner-images `/etc/environment` model, then configure the eventual runner's
`.env` once its binaries exist. Do not assume `/etc/environment` updates reach
every service automatically.

Ensure the runtime account can traverse the cache directories and read the
archives. Do not grant the job account cache-write access merely to get cache
hits; the runner copies and extracts cached files into its job work area.

## Windows VM image example

Run as an elevated image-provisioning process when using a machine-scoped cache.
Assume the runner is installed in `C:\actions-runner`.

```powershell
$ErrorActionPreference = 'Stop'
$cache = 'C:\actionarchivecache'
$runner = 'C:\actions-runner'

gh actions-archive-cache download actions/checkout `
    --latest-major-versions 2 --latest-versions-per-major 3 `
    --cache-dir $cache --format zip --hostname github.com
if ($LASTEXITCODE -ne 0) { throw 'Preloading checkout failed' }

gh actions-archive-cache download actions/setup-node `
    --latest-major-versions 2 --latest-versions-per-major 3 `
    --cache-dir $cache --format zip --hostname github.com
if ($LASTEXITCODE -ne 0) { throw 'Preloading setup-node failed' }

gh actions-archive-cache configure --scope system --cache-dir $cache
if ($LASTEXITCODE -ne 0) { throw 'Configuring the machine environment failed' }

gh actions-archive-cache configure --scope runner `
    --runner-path $runner --cache-dir $cache
if ($LASTEXITCODE -ne 0) { throw 'Configuring the runner environment failed' }
```

The machine setting matches the upstream Windows image installer. Runner scope
also makes the selected installation's startup environment explicit. Verify the
service account has read/traverse permission on the cache; avoid granting broad
write ACLs. Existing services need restarting before they see changed settings.

## macOS VM image example

Use the home directory of the **runtime runner account**, not an unrelated image
builder's home directory. This follows the upstream image's
`$HOME/actionarchivecache` convention while making service startup explicit.

```bash
set -euo pipefail

CACHE=/Users/runner/actionarchivecache
RUNNER=/Users/runner/actions-runner

gh actions-archive-cache download actions/checkout \
  --latest-major-versions 2 --latest-versions-per-major 3 \
  --cache-dir "$CACHE" --format tar.gz --hostname github.com

gh actions-archive-cache configure --scope runner \
  --runner-path "$RUNNER" --cache-dir "$CACHE"
```

`configure --scope user` uses shell exports, following runner-images' shell model.
Those are not a universal environment for launchd services. Prefer the selected
runner's `.env` for service-based images, and verify file access as that account.

## Preload a team's lockfile

Instead of guessing which action versions to include, copy a reviewed lockfile
from the workflows that will run on this image:

```bash
gh actions-archive-cache from-lockfile /build-inputs/actions.lock \
  --cache-dir /opt/actionarchivecache --format tar.gz \
  --hostname github.com
```

On Windows, use Windows paths and `--format zip`. Multiple lockfiles can be
imported with successive commands; archive/SHA duplicates are skipped.
Direct and recorded transitive dependencies are included. Import does not
re-resolve floating refs or rewrite the lockfile.

Use a reviewed lockfile generated by `github/gh-actions-lock`. An import is not
an independent trust/provenance verification of its contents.

An image can combine lockfile-pinned dependencies with a small rolling selection
of commonly used action versions. Pins imported into the cache metadata remain
protected during later cleanup.

## Container image provisioner

Use an ephemeral secret mount, not a token in `ARG`, `ENV`, a copied `hosts.yml`,
or a committed file. For example, with Docker BuildKit, in an existing Linux
runner Dockerfile that already contains `gh` and the built extension binary:

```dockerfile
# syntax=docker/dockerfile:1
# Runner binaries must already be present in /opt/actions-runner.
COPY actions.lock /build-inputs/actions.lock

RUN --mount=type=secret,id=github_token \
    GH_TOKEN="$(cat /run/secrets/github_token)" \
    /usr/local/bin/gh-actions-archive-cache from-lockfile /build-inputs/actions.lock \
      --hostname github.com --cache-dir /opt/actionarchivecache --format tar.gz

RUN /usr/local/bin/gh-actions-archive-cache configure --scope runner \
      --runner-path /opt/actions-runner --cache-dir /opt/actionarchivecache
```

Supply `github_token` using your build system's secret integration. Avoid shell
tracing around credentials. A secret mount prevents putting the secret itself
in a layer; the **downloaded action source is intentionally in the image**.
Never publish an image containing private action code to an audience that
should not have repository access.

Keep `/opt/actionarchivecache` in the final image if using a multi-stage build.
If the runner installation path changes between stages, run configuration for
the final path rather than relying on an old `.env`.

## GHE.com image builders

Select the tenant explicitly for its lockfiles and unlocked action discovery:

```bash
gh actions-archive-cache from-lockfile /build-inputs/actions.lock \
  --hostname TENANT.ghe.com --cache-dir /opt/actionarchivecache --format tar.gz
```

Have the build system inject both credentials when needed:

| Provisioner environment variable | Value |
|---|---|
| `GH_TOKEN` | Token for `TENANT.ghe.com` |
| `GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN` | Separate token for github.com |

`GITHUB_TOKEN` can substitute for `GH_TOKEN`, but `GH_TOKEN` takes precedence
when both are set. Without the separate github.com override, the extension
tries saved github.com authentication, then anonymous access; it does not reuse
the tenant environment token. The values are not automatically recognized or
validated as belonging to either host. Keep both out of image layers, logs,
and the final image.

Unlocked inputs check tenant repository existence first; lockfile host bindings
never fall back across hosts. Archives are downloaded directly from their
selected source host rather than through a tenant proxy. Allow access to
that source host and its archive redirects during image
provisioning. Do not assume all traffic remains inside the tenant.

## Refresh and control image size

Rebuilding from scratch naturally removes old image layers from the new image's
final cache. For a reused provisioning disk or persistent cache, preview
retention cleanup before deleting:

```bash
gh actions-archive-cache cleanup actions/checkout \
  --latest-major-versions 2 --latest-versions-per-major 3 \
  --cache-dir /opt/actionarchivecache --format tar.gz \
  --lockfile /build-inputs/actions.lock --dry-run
```

Remove `--dry-run` to perform the proposed deletions. Repeat for other common
action repositories. Run cleanup while runners using that cache are idle.
Keep the metadata index with the archives; it records identities and protects
known pins and explicit-ref downloads. Previously imported pins remain
conservatively protected even if a later lockfile omits them, so use a fresh
cache/image build when intentionally discarding that historical protection.

Unidentified files are kept and produce stderr warnings. Investigate them rather
than deleting the entire cache tree. The extension's operation lock does not
coordinate with an actively running Actions runner.

## Verify the resulting image

1. Run the same import/download twice during the build. The second run should
   report cached entries and no archive downloads. A fully cached pinned
   lockfile import makes no network requests.
2. Verify the final image retains the cache and the runner's `.env` setting,
   with paths correct for the runtime account.
3. Start an unregistered copy of the image, register it using fresh runtime
   credentials, and execute a representative workflow against the exact
   preloaded commits.
4. Inspect the runner's diagnostic logs for the existing archive lookup/copy
   messages and compare outbound archive requests with an uncached image.
   Cache-file layout checks alone do not prove a real runner cache hit.
5. Test a commit absent from the image to confirm ordinary download fallback
   still works, then refresh the image to cover newly adopted versions.

Actual runner-job cache-hit verification needs an available configured runner;
it is a separate image acceptance check.
