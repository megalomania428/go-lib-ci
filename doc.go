// Package ci provides reusable helpers for CI/CD Go programs: apt package
// installation with an inter-process lock and downloading GitHub release
// assets with retries, sha256 verification, progress bar and a curl-like
// low-speed watchdog. Both public and private repositories are supported.
package ci
