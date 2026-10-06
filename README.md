# Golang library for CI/CD workflows

<!-- cspell:ignore LZMA strconv Fprintln Getenv Println -->
<!-- markdownlint-disable MD010 MD013 -->

[![lib-test](https://github.com/megalomania428/go-lib-ci/actions/workflows/repo-test.yaml/badge.svg)](https://github.com/megalomania428/go-lib-ci/actions/workflows/repo-test.yaml)

## Environment helpers

`EnvDefault`, `ParseIntEnv` and `ParseDurationEnv` read an environment
variable and fall back to a default when it is unset or empty, parsing ints
and `time.Duration` values respectively and wrapping unparseable input in a
descriptive error.

## Download a URL

`FetchURL` downloads a single HTTP/HTTPS URL to a destination path with
retries, a low-speed watchdog, resumable partial downloads, optional sha256
verification and the same curl-like progress bar as uploads. `Retry` and
`NewHTTPClient` are the lower-level building blocks it is built on and can be
reused directly for custom retry loops or HTTP clients tuned for CI reliability.

## Download a GitHub release asset

`FetchGitHubRelease` downloads a named asset from a GitHub release (latest or
a specific tag), skipping the download when a local copy already matches the
release digest, and optionally falling back to an existing file when the API
or download fails after retries.

## Molecule test setup

`Prepare` performs the common molecule test bootstrap shared by the
raven428 ansible role repositories: fetching the ansible AppImage, resolving
`ANSIBLE_ROLES_PATH` and symlinking the role source into it. `MoleculeCreate`
and `RunGroup` wrap the corresponding `molecule` CLI invocations, and
`CloneRoleRefs` overrides galaxy-installed roles with specific git refs from
environment variables.

## Ensure apt packages

`EnsurePackages` checks the dpkg status database for a list of Debian
packages and installs whichever are missing via `sudo apt-get`, serializing
concurrent callers through a file lock.

## Read a Vault KV v2 secret

`NewVaultClient` builds a client for the Vault HTTP API, `ParseVaultKVv2Path`
splits a `<mount>/<path>/<key>` string into a `VaultSecretRef`, and
`(*VaultClient).ReadVaultKVv2` fetches the string value of that key from a
KV v2 secret mount.

## Upload progress

`NewProgressBar` exposes the same curl-like progress bar `FetchURL` uses
for downloads, so it can be wrapped around any `io.Reader` for uploads.
`Reverse: true` fills the bar right-to-left, matching bytes leaving to
the network:

```go
size, err := body.Seek(0, io.SeekEnd)
if err != nil {
  return err
}
if _, err := body.Seek(0, io.SeekStart); err != nil {
  return err
}
bar := ci.NewProgressBar(ctx, ci.ProgressOptions{
  Name:    filepath.Base(pkgPath),
  Total:   size,
  Stderr:  os.Stderr,
  Reverse: true,
})
req.ContentLength = size
req.Body = bar.WrapReader(body)
resp, err := client.Do(req)
if err == nil {
  defer resp.Body.Close()
  if resp.StatusCode >= http.StatusBadRequest {
    err = fmt.Errorf("upload failed: %s", resp.Status)
  }
}
bar.Finish(err)
```

When the stream size is not known yet at bar creation time, pass `Total: -1`
and call `SetTotal` once it becomes available — this is how `FetchURL` covers
connect/TLS with a wave animation before `Content-Length` arrives.

## Functional-option APIs

New APIs accept `ctx context.Context` first and optional `With…` functions for all remaining settings. Constructors do not need a context. Missing required options wrap `ErrMissingOption`, allowing `errors.Is(err, ci.ErrMissingOption)`. Existing positional APIs are unchanged.

### Commands and Git

`RunCommand` inherits the environment and supports working directory, additional environment entries and input/output streams. `CommandOutput` captures stdout, regardless of `WithCommandStdout`, while stderr remains separate. Errors include the executable and exit status, but omit arguments to avoid exposing credentials. External executable names are overridable, including Git, patch and archive tools.

`GitClone` pins a tag or branch, optionally using depth, bare mode and a partial-clone filter. `GitChangedFiles` supports merge-base comparisons and disables rename detection so moves include both old and new paths. `GitPreviousTag` finds the nearest matching tag reachable from `Ref`, excluding `Ref` itself with `git describe --tags --abbrev=0 --match <Match> --exclude <Ref> --end-of-options <Ref>`, where the `refs/tags/` prefix is stripped from the `--exclude` value because Git matches it against short tag names; another matching tag on the same commit is eligible. No matching tag returns an empty string without an error; other Git failures remain errors. `GitDiff`, `GitLog` and `GitShow` accept path restrictions. Filenames containing whitespace are retained by NUL-delimited changed-file parsing.

### Patches and archives

`ApplyPatches` applies regular `.diff` and `.patch` files in directory order and lexical filename order with `patch --batch --forward`. Missing directories and already applied patches are errors; empty directories are allowed. Returned filenames record successful applications before an error.

`ArchiveTarXz` uses `tar` with `xz -T0 -9e`; `Archive7z` uses LZMA2 at maximum compression. Both remove the previous archive first and store the source by its base name, not its absolute path. `ArchiveTarXz` rejects output paths inside the source before removing an existing archive. Archive programs and system packages must already be installed; installation belongs to the caller, for example through `EnsurePackages`.

### GitHub releases

`NewGitHubClient` uses `net/http` with GitHub API headers and configurable REST/upload endpoints, HTTP client, backoff, progress and stderr. `FindRelease` returns nil for an absent published release, or the lowest-ID matching draft across all pages. `EnsureRelease` updates explicitly supplied, differing name/body fields (including empty strings), preserves omitted fields on existing releases and leaves them out when creating a new one, handles a published-release create race and converges concurrent draft creators by deleting their newer duplicate. `ListAssets` follows Link pagination. `UploadAsset` optionally deletes only matching names, reports reversed upload progress and reopens the file on rate-limit responses (429, or 403 with `Retry-After` or exhausted quota) and connection failures. Other transport failures, HTTP 408/5xx and unreadable upload responses stop without retrying because the server may already have accepted the asset. With `WithAssetReplace(true)`, a 422 `already_exists` response repeats matching-name cleanup and upload, for at most three upload cycles; without replacement it remains an error. `DeleteAsset` and `DeleteRelease` tolerate 404. Errors retain `*GitHubAPIError` for `errors.As`.

Network failures, 5xx, 408, 429 and rate-limit 403 responses use `Retry`, honoring `Retry-After` and `X-RateLimit-Reset`; other HTTP failures stop immediately. POST requests (release creation and uploads) retry only connection failures and rate limits, because after other transport failures, 408/5xx or unreadable responses GitHub may already have applied them. The token requires appropriate repository permissions. A draft's unrelated assets are never automatically removed.

### Telegram Rich Messages

`NewTelegramClient` uses `github.com/go-telegram/bot` with getMe disabled. `SendRichPost` sends one Rich Markdown post with documents referenced at the end through `tg://document?id=` and uploaded through multipart `attach://` parts. Documents are reopened on every retry and duplicate base names get distinct part names. The caller enforces file/media/text limits and decides whether to retry permanent API rejection with fallback text. Connection failures and 429 use `Retry`, honoring Telegram `retry_after`; other network, 5xx and response errors stop because the post may already be delivered. API errors retain `*TelegramAPIError`. Server URL and HTTP client are overridable for mock-server tests.

## Make release

- clone me:

```bash
git clone --recursive git@github.com:megalomania428/go-lib-ci.git go-lib-ci
```

- make tag and send to release:

```bash
git checkout master && git pull
git tag -fm $(git branch --sho) v1.0.6 && git push --force origin $(git describe)
```
