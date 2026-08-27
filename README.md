# Golang library for CI/CD workflows

[![lib-test](https://github.com/megalomania428/go-lib-ci/actions/workflows/lib-test.yaml/badge.svg)](https://github.com/megalomania428/go-lib-ci/actions/workflows/lib-test.yaml)

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

## Make release

- clone me:

```bash
git clone --recursive git@github.com:megalomania428/go-lib-ci.git go-lib-ci
```

- make tag and send to release:

```bash
git checkout master && git pull
git tag -fm $(git branch --sho) v1.0.4 && git push --force origin $(git describe)
```
