// Package ci is documented in doc.go.
package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FetchOptions controls FetchGitHubRelease.
type FetchOptions struct {
	// Repo is "owner/name" of the GitHub repository.
	Repo string
	// Tag identifies the release. Empty means "latest".
	Tag string
	// AssetName is the exact asset file name to download.
	AssetName string
	// DestDir is the target directory. The final path is DestDir/AssetName.
	// When empty, the current working directory is used.
	DestDir string
	// FileMode is applied to the downloaded file. Defaults to 0o755.
	FileMode os.FileMode
	// Token is a GitHub token used for authentication. Required for private
	// repositories.
	Token string
	// SkipVerify disables sha256 verification against the asset .digest
	// field returned by the API. When false (the default) and the API
	// exposes a digest, the downloaded file is verified and a mismatch
	// causes an error.
	SkipVerify bool
	// FallbackToExisting returns the existing file at DestDir/AssetName when
	// either the API request or the download fails after all retries.
	FallbackToExisting bool
	// ConnectTimeout limits the TCP connect, TLS handshake and response. Zero
	// keeps the default of 11 seconds.
	ConnectTimeout time.Duration
	// StallTimeout aborts the transfer if less than StallLimit bytes have
	// been received during that window. Zero keeps the default of 30
	// seconds. Set to a negative value to disable the watchdog.
	StallTimeout time.Duration
	// StallLimit is the minimum acceptable throughput in bytes per
	// StallTimeout window. Zero keeps the default of 1 byte.
	StallLimit int64
	// BackoffOptions carries the retry/backoff schedule knobs shared with
	// DownloadOptions and RetryOptions.
	BackoffOptions
	// HTTPClient overrides the default *http.Client.
	HTTPClient *http.Client
	// APIBaseURL overrides the GitHub API endpoint. Defaults to
	// https://api.github.com.
	APIBaseURL string
	// NoProgressBar disables the curl-like progress bar. By default the
	// progress bar is drawn on Stderr regardless of whether it is a TTY.
	NoProgressBar bool
	// CIMode forces the curl-like non-TTY progress renderer that prints
	// every frame on its own line, suitable for CI logs. When nil the
	// renderer is picked automatically: mpb for TTY, CI renderer
	// otherwise. Explicit true/false overrides the auto-detection.
	CIMode *bool
	// Stdout receives informational messages. Defaults to os.Stdout.
	Stdout io.Writer
	// Stderr receives error and progress output. Defaults to os.Stderr.
	Stderr io.Writer
}

const (
	defaultAPIBaseURL = "https://api.github.com"
	githubAcceptV3    = "application/vnd.github+json"
	githubAPIVersion  = "2022-11-28"
)

type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Digest string `json:"digest"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// downloadOptions builds the transport-agnostic DownloadOptions carried by the
// generic downloader from the GitHub-specific FetchOptions.
func (opts *FetchOptions) downloadOptions(wantDigest string) DownloadOptions {
	token := opts.Token
	return DownloadOptions{
		Name:       opts.AssetName,
		FileMode:   opts.FileMode,
		WantDigest: wantDigest,
		SetHeaders: func(req *http.Request) {
			setGitHubHeaders(req, "application/octet-stream", token)
		},
		ConnectTimeout: opts.ConnectTimeout,
		StallTimeout:   opts.StallTimeout,
		StallLimit:     opts.StallLimit,
		BackoffOptions: opts.BackoffOptions,
		HTTPClient:     opts.HTTPClient,
		NoProgressBar:  opts.NoProgressBar,
		CIMode:         opts.CIMode,
		Stdout:         opts.Stdout,
		Stderr:         opts.Stderr,
	}
}

// FetchGitHubRelease downloads a single named asset from a GitHub release.
// It returns the absolute path to the local file. Retries, low-speed watchdog
// and sha256 verification (when the API exposes it) are applied automatically.
// The AssetName must match exactly; no substitutions are performed.
func FetchGitHubRelease(ctx context.Context, opts FetchOptions) (string, error) {
	if opts.Repo == "" {
		return "", errors.New(`repo required ("owner/name")`)
	}
	if !strings.Contains(opts.Repo, "/") {
		return "", fmt.Errorf(`repo must be "owner/name", got %q`, opts.Repo)
	}
	if opts.AssetName == "" {
		return "", errors.New("AssetName required")
	}
	// Delegate the defaults shared with DownloadOptions to
	// applyDownloadDefaults instead of duplicating the same nine
	// zero-value checks here.
	dl := DownloadOptions{
		FileMode:       opts.FileMode,
		ConnectTimeout: opts.ConnectTimeout,
		StallTimeout:   opts.StallTimeout,
		StallLimit:     opts.StallLimit,
		BackoffOptions: opts.BackoffOptions,
		HTTPClient:     opts.HTTPClient,
		Stdout:         opts.Stdout,
		Stderr:         opts.Stderr,
	}
	applyDownloadDefaults(&dl)
	opts.FileMode = dl.FileMode
	opts.ConnectTimeout = dl.ConnectTimeout
	opts.StallTimeout = dl.StallTimeout
	opts.StallLimit = dl.StallLimit
	opts.BackoffOptions = dl.BackoffOptions
	opts.HTTPClient = dl.HTTPClient
	opts.Stdout = dl.Stdout
	opts.Stderr = dl.Stderr
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = defaultAPIBaseURL
	} else {
		opts.APIBaseURL = strings.TrimRight(opts.APIBaseURL, "/")
	}
	destDir := opts.DestDir
	if destDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		destDir = wd
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", destDir, err)
	}
	dest := filepath.Join(destDir, opts.AssetName)
	verify := !opts.SkipVerify
	asset, err := fetchAssetWithRetry(ctx, &opts)
	if err != nil {
		if opts.FallbackToExisting && fileExists(dest) {
			fmt.Fprintf(
				opts.Stderr, "==> %s: API unavailable, using existing binary\n",
				opts.AssetName,
			)
			return dest, nil
		}
		return "", err
	}
	remoteDigest := parseDigest(asset.Digest)
	if verify && remoteDigest != "" {
		if fileExists(dest) {
			local, err := sha256File(dest)
			if err == nil && local == remoteDigest {
				fmt.Fprintf(
					opts.Stdout, "==> %s: up to date, skipping download\n",
					opts.AssetName,
				)
				return dest, nil
			}
		}
	}
	if asset.URL == "" {
		return "", errors.New("empty asset URL in release payload")
	}
	dlOpts := opts.downloadOptions(remoteDigest)
	if err := downloadWithRetry(ctx, &dlOpts, asset.URL, dest); err != nil {
		if opts.FallbackToExisting && fileExists(dest) {
			fmt.Fprintf(
				opts.Stderr, "==> %s: download failed, using existing binary\n",
				opts.AssetName,
			)
			return dest, nil
		}
		return "", err
	}
	return dest, nil
}

func fetchAssetWithRetry(ctx context.Context, opts *FetchOptions) (*ghAsset, error) {
	var asset *ghAsset
	retryOpts := RetryOptions{
		Name:           opts.AssetName + ": API",
		BackoffOptions: opts.BackoffOptions,
		Stderr:         opts.Stderr,
		IdleCloser:     transportIdleCloser(opts.HTTPClient),
	}
	err := Retry(ctx, retryOpts, func(ctx context.Context) error {
		a, err := fetchAsset(ctx, opts)
		if err != nil {
			return err
		}
		asset = a
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("fetch release metadata: %w", err)
	}
	return asset, nil
}

func setGitHubHeaders(req *http.Request, accept, token string) {
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func fetchAsset(ctx context.Context, opts *FetchOptions) (*ghAsset, error) {
	var url string
	if opts.Tag == "" {
		url = fmt.Sprintf(
			"%s/repos/%s/releases/latest", opts.APIBaseURL, opts.Repo,
		)
	} else {
		url = fmt.Sprintf(
			"%s/repos/%s/releases/tags/%s", opts.APIBaseURL, opts.Repo, opts.Tag,
		)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	setGitHubHeaders(req, githubAcceptV3, opts.Token)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf(
			"GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)),
		)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release payload: %w", err)
	}
	for i := range rel.Assets {
		if rel.Assets[i].Name == opts.AssetName {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf(
		"asset %q not found in release %s of %s",
		opts.AssetName, tagLabel(opts.Tag), opts.Repo,
	)
}

func parseDigest(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "sha256:") {
		return strings.TrimPrefix(raw, "sha256:")
	}
	return ""
}

func tagLabel(tag string) string {
	if tag == "" {
		return "latest"
	}
	return tag
}
