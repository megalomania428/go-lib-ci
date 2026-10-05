// Package ci is documented in doc.go.
package ci

// cspell:ignore htmlurl commitish apihttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// responseStallTimeout allows tests to trigger a silent response body quickly.
var responseStallTimeout = defaultStallTimeout

// responseReadTimeout caps the whole body read; tests shorten it to hit the deadline.
var responseReadTimeout = 2 * time.Minute

// requestWriteTimeout fails a request write that makes no progress; tests shorten it.
var requestWriteTimeout = defaultStallTimeout

// releaseProgressBar allows tests to inspect the final upload error.
var releaseProgressBar = NewProgressBar

// githubPause allows tests to observe rate-limit waits without sleeping.
var githubPause = waitTelegram

// Release is GitHub release metadata.
type Release struct {
	// ID is the repository-local release identifier.
	ID int64 `json:"id"`
	// TagName is the release's Git tag.
	TagName string `json:"tag_name"`
	// Name is the human-readable release title.
	Name string `json:"name"`
	// Body is the Markdown release description.
	Body string `json:"body"`
	// Draft indicates an unpublished release.
	Draft bool `json:"draft"`
	// HTMLURL is the browser URL of the release.
	HTMLURL string `json:"html_url"`
	// UploadURL is the upload URI template returned by GitHub.
	UploadURL string `json:"upload_url"`
	// Assets contains the release's attached asset metadata.
	Assets []Asset `json:"assets"`
}

// Asset is GitHub release asset metadata.
type Asset struct {
	// ID is the repository-local asset identifier.
	ID int64 `json:"id"`
	// Name is the uploaded file name.
	Name string `json:"name"`
	// Size is the file size in bytes.
	Size int64 `json:"size"`
	// BrowserDownloadURL is GitHub's browser-facing asset URL.
	BrowserDownloadURL string `json:"browser_download_url"`
}

// GitHubAPIError represents an unsuccessful GitHub API response.
type GitHubAPIError struct {
	// StatusCode is the unsuccessful HTTP status.
	StatusCode int
	// Message contains the diagnostic response body.
	Message string
}

// Error returns the API status and diagnostic.
func (e *GitHubAPIError) Error() string {
	return fmt.Sprintf("GitHub API %d: %s", e.StatusCode, e.Message)
}

// GitHubClient publishes releases using net/http.
type GitHubClient struct {
	repo, token, api, upload string
	http                     *http.Client
	backoff                  BackoffOptions
	noProgress               bool
	stderr                   io.Writer
}

// GitHubOption configures NewGitHubClient.
type GitHubOption func(*GitHubClient)

// WithGitHubRepo sets the required owner/name repository.
func WithGitHubRepo(ownerName string) GitHubOption {
	return func(c *GitHubClient) { c.repo = ownerName }
}

// WithGitHubToken sets the required API token.
func WithGitHubToken(token string) GitHubOption {
	return func(c *GitHubClient) { c.token = token }
}

// WithGitHubAPIBaseURL overrides the REST endpoint.
func WithGitHubAPIBaseURL(u string) GitHubOption {
	return func(c *GitHubClient) { c.api = strings.TrimRight(u, "/") }
}

// WithGitHubUploadBaseURL overrides the asset upload endpoint.
func WithGitHubUploadBaseURL(u string) GitHubOption {
	return func(c *GitHubClient) { c.upload = strings.TrimRight(u, "/") }
}

// WithGitHubHTTPClient overrides the HTTP client.
func WithGitHubHTTPClient(h *http.Client) GitHubOption {
	return func(c *GitHubClient) { c.http = h }
}

// WithGitHubBackoff overrides the retry schedule.
func WithGitHubBackoff(b BackoffOptions) GitHubOption {
	return func(c *GitHubClient) { c.backoff = b }
}

// WithGitHubNoProgressBar disables upload progress.
func WithGitHubNoProgressBar(on bool) GitHubOption {
	return func(c *GitHubClient) { c.noProgress = on }
}

// WithGitHubStderr sets diagnostics and progress output.
func WithGitHubStderr(w io.Writer) GitHubOption {
	return func(c *GitHubClient) { c.stderr = w }
}

// NewGitHubClient validates options without making any requests.
func NewGitHubClient(opts ...GitHubOption) (*GitHubClient, error) {
	c := &GitHubClient{api: defaultAPIBaseURL, upload: "https://uploads.github.com",
		http: newAPIHTTPClient(), stderr: os.Stderr}
	for _, opt := range opts {
		opt(c)
	}
	if c.repo == "" {
		return nil, fmt.Errorf("NewGitHubClient: repo: %w", ErrMissingOption)
	}
	parts := strings.Split(c.repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("GitHub repository must be owner/name: %q", c.repo)
	}
	if strings.TrimSpace(c.token) == "" {
		return nil, fmt.Errorf("NewGitHubClient: token: %w", ErrMissingOption)
	}
	if c.http == nil {
		c.http = newAPIHTTPClient()
	}
	if c.stderr == nil {
		c.stderr = os.Stderr
	}
	return c, nil
}

// newAPIHTTPClient bounds every request write, because ResponseHeaderTimeout starts only
// after the body is sent and a peer that stops reading an upload would block Do forever.
func newAPIHTTPClient() *http.Client {
	h := NewHTTPClient(defaultConnectTimeout)
	tr, _ := h.Transport.(*http.Transport)
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &writeDeadlineConn{Conn: conn, timeout: requestWriteTimeout}, nil
	}
	return h
}

// writeDeadlineConn fails a single write that stays blocked for longer than timeout.
type writeDeadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *writeDeadlineConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

type releaseConfig struct {
	tag              string
	name             string
	body             string
	draft            bool
	target           string
	id               int64
	nameSet, bodySet bool
}

// ReleaseOption configures release operations.
type ReleaseOption func(*releaseConfig)

// WithReleaseTag sets the required tag for lookup or creation.
func WithReleaseTag(tag string) ReleaseOption {
	return func(c *releaseConfig) { c.tag = tag }
}

// WithReleaseName sets the release title; omission preserves an existing title.
func WithReleaseName(name string) ReleaseOption {
	return func(c *releaseConfig) { c.name, c.nameSet = name, true }
}

// WithReleaseBody sets Markdown release notes; omission preserves existing notes.
func WithReleaseBody(body string) ReleaseOption {
	return func(c *releaseConfig) { c.body, c.bodySet = body, true }
}

// WithReleaseDraft selects draft lookup and creation.
func WithReleaseDraft(draft bool) ReleaseOption {
	return func(c *releaseConfig) { c.draft = draft }
}

// WithReleaseTarget sets target_commitish on creation.
func WithReleaseTarget(commitish string) ReleaseOption {
	return func(c *releaseConfig) { c.target = commitish }
}

// WithReleaseID sets the required ID for deletion.
func WithReleaseID(id int64) ReleaseOption { return func(c *releaseConfig) { c.id = id } }
func releaseOptions(opts []ReleaseOption) *releaseConfig {
	c := &releaseConfig{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}
func (c *GitHubClient) endpoint(suffix string) string {
	return c.api + "/repos/" + c.repo + "/releases" + suffix
}

// FindRelease finds a published release or the lowest-ID draft with the tag.
func (c *GitHubClient) FindRelease(ctx context.Context,
	opts ...ReleaseOption) (*Release, error) {
	r := releaseOptions(opts)
	if r.tag == "" {
		return nil, fmt.Errorf("FindRelease: tag: %w", ErrMissingOption)
	}
	if !r.draft {
		var rel Release
		_, err := c.request(ctx, http.MethodGet, c.endpoint("/tags/"+url.PathEscape(r.tag)),
			nil, &rel, "", 0)
		if isGitHubStatus(err, http.StatusNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &rel, nil
	}
	var found *Release
	releases, err := getPages[Release](ctx, c, c.endpoint("?per_page=100"))
	if err != nil {
		return nil, err
	}
	for i := range releases {
		rel := &releases[i]
		if rel.Draft && rel.TagName == r.tag && (found == nil || rel.ID < found.ID) {
			found = rel
		}
	}
	return found, nil
}

// EnsureRelease creates or updates a release, converging racing draft creators.
func (c *GitHubClient) EnsureRelease(ctx context.Context,
	opts ...ReleaseOption) (*Release, error) {
	r := releaseOptions(opts)
	rel, err := c.FindRelease(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if rel != nil {
		return c.updateRelease(ctx, rel, r)
	}
	body := map[string]any{"tag_name": r.tag, "draft": r.draft}
	if r.nameSet {
		body["name"] = r.name
	}
	if r.bodySet {
		body["body"] = r.body
	}
	if r.target != "" {
		body["target_commitish"] = r.target
	}
	payload, _ := json.Marshal(body)
	var created Release
	_, err = c.request(ctx, http.MethodPost, c.endpoint(""), payload, &created, "", 0)
	if err != nil {
		var api *GitHubAPIError
		if !r.draft && errors.As(err, &api) && api.StatusCode == 422 &&
			strings.Contains(api.Message, "already_exists") {
			rel, findErr := c.FindRelease(ctx, opts...)
			if findErr != nil {
				return nil, findErr
			}
			if rel != nil {
				return c.updateRelease(ctx, rel, r)
			}
		}
		return nil, err
	}
	if r.draft {
		lowest, err := c.FindRelease(ctx, opts...)
		if err != nil {
			return nil, err
		}
		if lowest != nil && lowest.ID < created.ID {
			if err := c.DeleteRelease(ctx, WithReleaseID(created.ID)); err != nil {
				return nil, err
			}
			return c.updateRelease(ctx, lowest, r)
		}
	}
	return &created, nil
}
func (c *GitHubClient) updateRelease(ctx context.Context, rel *Release,
	r *releaseConfig) (*Release, error) {
	body := map[string]string{}
	if r.nameSet && rel.Name != r.name {
		body["name"] = r.name
	}
	if r.bodySet && rel.Body != r.body {
		body["body"] = r.body
	}
	if len(body) == 0 {
		return rel, nil
	}
	payload, _ := json.Marshal(body)
	var updated Release
	endpoint := c.endpoint("/" + strconv.FormatInt(rel.ID, 10))
	_, err := c.request(ctx, http.MethodPatch, endpoint, payload, &updated, "", 0)
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

type assetConfig struct {
	release, id int64
	path, name  string
	replace     bool
}

// AssetOption configures asset operations.
type AssetOption func(*assetConfig)

// WithAssetRelease sets the required release ID for listing or uploading.
func WithAssetRelease(id int64) AssetOption {
	return func(c *assetConfig) { c.release = id }
}

// WithAssetPath sets the required upload file.
func WithAssetPath(path string) AssetOption {
	return func(c *assetConfig) { c.path = path }
}

// WithAssetName overrides the uploaded base name.
func WithAssetName(name string) AssetOption {
	return func(c *assetConfig) { c.name = name }
}

// WithAssetReplace deletes only matching names and retries duplicate-name conflicts.
func WithAssetReplace(on bool) AssetOption {
	return func(c *assetConfig) { c.replace = on }
}

// WithAssetID sets the required asset ID for deletion.
func WithAssetID(id int64) AssetOption { return func(c *assetConfig) { c.id = id } }
func assetOptions(opts []AssetOption) *assetConfig {
	c := &assetConfig{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ListAssets follows Link pagination for a release's assets.
func (c *GitHubClient) ListAssets(ctx context.Context,
	opts ...AssetOption) ([]Asset, error) {
	a := assetOptions(opts)
	if a.release <= 0 {
		return nil, fmt.Errorf("ListAssets: release: %w", ErrMissingOption)
	}
	return getPages[Asset](ctx, c,
		c.endpoint("/"+strconv.FormatInt(a.release, 10)+"/assets?per_page=100"))
}

// getPages follows Link pagination and fails when a next URL repeats.
func getPages[T any](ctx context.Context, c *GitHubClient, next string) ([]T, error) {
	var items []T
	seen := map[string]bool{}
	for next != "" {
		if seen[next] {
			return nil, fmt.Errorf("GitHub pagination loop at %s", next)
		}
		seen[next] = true
		var page []T
		var err error
		next, err = c.request(ctx, http.MethodGet, next, nil, &page, "", 0)
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
	}
	return items, nil
}

// UploadAsset reopens the file on rate-limit or connection failures and reports progress.
// Transport, timeout, server and response-reading errors stop to avoid duplicate uploads.
// Replacement allows at most three cleanup/upload cycles on duplicate-name errors.
func (c *GitHubClient) UploadAsset(ctx context.Context,
	opts ...AssetOption) (*Asset, error) {
	a := assetOptions(opts)
	if a.release <= 0 {
		return nil, fmt.Errorf("UploadAsset: release: %w", ErrMissingOption)
	}
	if a.path == "" {
		return nil, fmt.Errorf("UploadAsset: path: %w", ErrMissingOption)
	}
	if a.name == "" {
		a.name = filepath.Base(a.path)
	}
	info, err := os.Stat(a.path)
	if err != nil {
		return nil, fmt.Errorf("stat asset: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("asset is not a file: %s", a.path)
	}
	u := c.upload + "/repos/" + c.repo + "/releases/" + strconv.FormatInt(a.release, 10) +
		"/assets?name=" + url.QueryEscape(a.name)
	for range 3 {
		if a.replace {
			assets, err := c.ListAssets(ctx, WithAssetRelease(a.release))
			if err != nil {
				return nil, err
			}
			for _, asset := range assets {
				if asset.Name == a.name {
					if err := c.DeleteAsset(ctx, WithAssetID(asset.ID)); err != nil {
						return nil, err
					}
				}
			}
		}
		var asset Asset
		_, err = c.request(ctx, http.MethodPost, u, nil, &asset, a.path, info.Size())
		if err == nil {
			return &asset, nil
		}
		var api *GitHubAPIError
		if !a.replace || !errors.As(err, &api) || api.StatusCode != 422 ||
			!strings.Contains(api.Message, "already_exists") {
			return nil, err
		}
	}
	return nil, err
}

// DeleteAsset deletes an asset; an already absent asset is not an error.
func (c *GitHubClient) DeleteAsset(ctx context.Context, opts ...AssetOption) error {
	a := assetOptions(opts)
	if a.id <= 0 {
		return fmt.Errorf("DeleteAsset: id: %w", ErrMissingOption)
	}
	_, err := c.request(ctx, http.MethodDelete,
		c.endpoint("/assets/"+strconv.FormatInt(a.id, 10)), nil, nil, "", 0)
	if isGitHubStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// DeleteRelease deletes a release; an already absent release is not an error.
func (c *GitHubClient) DeleteRelease(ctx context.Context, opts ...ReleaseOption) error {
	r := releaseOptions(opts)
	if r.id <= 0 {
		return fmt.Errorf("DeleteRelease: id: %w", ErrMissingOption)
	}
	_, err := c.request(ctx, http.MethodDelete,
		c.endpoint("/"+strconv.FormatInt(r.id, 10)), nil, nil, "", 0)
	if isGitHubStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
func isGitHubStatus(err error, code int) bool {
	var api *GitHubAPIError
	return errors.As(err, &api) && api.StatusCode == code
}

// stopRetry allows new APIs to short-circuit Retry without changing its public API.
type stopRetry struct{ error }

// retryPause asks retryAPI to wait a server-requested delay before the next attempt.
type retryPause struct {
	error
	delay time.Duration
}

// retryAPI keeps Retry backoff sleeps and server-requested pauses within one
// MaxRetriesTime budget, because Retry itself only accounts for its backoff.
func retryAPI(ctx context.Context, opts RetryOptions,
	pause func(context.Context, time.Duration) error, fn func(context.Context) error) error {
	applyRetryDefaults(&opts)
	var permanent error
	var waited, pending time.Duration
	attempt := 0
	err := Retry(ctx, opts, func(ctx context.Context) error {
		if pending > 0 {
			if err := pause(ctx, pending); err != nil {
				permanent = err
				return nil
			}
		}
		pending = 0
		err := fn(ctx)
		if err == nil {
			return nil
		}
		var stop *stopRetry
		if errors.As(err, &stop) {
			permanent = stop.error
			return nil
		}
		var extra time.Duration
		var requested *retryPause
		if errors.As(err, &requested) {
			err, extra = requested.error, max(requested.delay, 0)
		}
		wait := backoffDelay(attempt, &opts) + extra
		attempt++
		if waited+wait > opts.MaxRetriesTime {
			permanent = err
			return nil
		}
		waited += wait
		pending = extra
		return err
	})
	if permanent != nil {
		return permanent
	}
	return err
}

// requestUnsent reports a failed connection, so the server never received the request.
func requestUnsent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// stallBody cancels the request when the response body stays silent for longer than
// timeout, because ResponseHeaderTimeout no longer applies once headers arrive. The
// deadline is never reset, so a server dripping bytes cannot extend the read forever.
type stallBody struct {
	io.ReadCloser
	timeout, limit  time.Duration
	timer, deadline *time.Timer
	stalled         atomic.Bool
	expired         atomic.Bool
}

func newStallBody(body io.ReadCloser, cancel context.CancelFunc) *stallBody {
	b := &stallBody{ReadCloser: body, timeout: responseStallTimeout,
		limit: responseReadTimeout}
	b.timer = time.AfterFunc(b.timeout, func() {
		b.stalled.Store(true)
		cancel()
	})
	b.deadline = time.AfterFunc(b.limit, func() {
		b.expired.Store(true)
		cancel()
	})
	return b
}
func (b *stallBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.timeout)
	}
	if err != nil && b.stalled.Load() {
		err = fmt.Errorf("response stalled for %s: %w", b.timeout, err)
	} else if err != nil && b.expired.Load() {
		err = fmt.Errorf("response read exceeded %s: %w", b.limit, err)
	}
	return n, err
}
func (b *stallBody) Close() error {
	b.timer.Stop()
	b.deadline.Stop()
	return b.ReadCloser.Close()
}
func (c *GitHubClient) request(ctx context.Context, method, u string, payload []byte,
	out any, file string, size int64) (string, error) {
	var next string
	// POST is not idempotent, so a lost response must not trigger a duplicate.
	once := method == http.MethodPost
	err := retryAPI(ctx, RetryOptions{Name: "GitHub " + method, BackoffOptions: c.backoff,
		Stderr: c.stderr, IdleCloser: transportIdleCloser(c.http)}, githubPause,
		func(ctx context.Context) (err error) {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
			if err != nil {
				return &stopRetry{fmt.Errorf("create GitHub request: %w", err)}
			}
			setGitHubHeaders(req, githubAcceptV3, c.token)
			req.Header.Set("Content-Type", "application/json")
			if file != "" {
				f, openErr := os.Open(file)
				if openErr != nil {
					return &stopRetry{fmt.Errorf("open asset: %w", openErr)}
				}
				defer func() { _ = f.Close() }()
				bar := releaseProgressBar(ctx, ProgressOptions{Name: filepath.Base(file),
					Total: size, Reverse: true, NoProgressBar: c.noProgress, Stderr: c.stderr})
				defer func() { bar.Finish(err) }()
				req.Body = bar.WrapReader(f)
				req.ContentLength = size
				if size == 0 {
					req.Body = http.NoBody
				}
				req.Header.Set("Content-Type", "application/octet-stream")
			}
			resp, err := c.http.Do(req)
			if err != nil {
				err = fmt.Errorf("GitHub request: %w", err)
				if once && !requestUnsent(err) {
					return &stopRetry{err}
				}
				return err
			}
			resp.Body = newStallBody(resp.Body, cancel)
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				body, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
				if err != nil {
					err = fmt.Errorf("read GitHub error: %w", err)
					if once {
						return &stopRetry{err}
					}
					return err
				}
				api := &GitHubAPIError{resp.StatusCode, strings.TrimSpace(string(body))}
				if once && (resp.StatusCode >= 500 || resp.StatusCode == 408) {
					return &stopRetry{api}
				}
				limited, delay := githubRateLimit(resp.Header)
				if resp.StatusCode == 429 || (resp.StatusCode == 403 && limited) {
					return &retryPause{api, delay}
				}
				if resp.StatusCode >= 500 || resp.StatusCode == 408 {
					return api
				}
				return &stopRetry{api}
			}
			if out != nil {
				if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
					err = fmt.Errorf("decode GitHub response: %w", err)
					if once {
						return &stopRetry{err}
					}
					return err
				}
			}
			next = nextPage(resp.Header.Get("Link"), u)
			return nil
		})
	return next, err
}

// githubRateLimit reports a primary or secondary rate limit and the wait GitHub requests.
func githubRateLimit(h http.Header) (bool, time.Duration) {
	if after := h.Get("Retry-After"); after != "" {
		seconds, _ := strconv.Atoi(after)
		return true, time.Duration(seconds) * time.Second
	}
	if h.Get("X-RateLimit-Remaining") != "0" {
		return false, 0
	}
	reset, _ := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	return true, time.Until(time.Unix(reset, 0))
}
func nextPage(link, current string) string {
	for _, part := range strings.Split(link, ",") {
		fields := strings.Split(part, ";")
		if len(fields) < 2 {
			continue
		}
		for _, field := range fields[1:] {
			if strings.TrimSpace(field) != `rel="next"` {
				continue
			}
			raw := strings.Trim(strings.TrimSpace(fields[0]), "<>")
			base, err := url.Parse(current)
			if err != nil {
				return ""
			}
			next, err := url.Parse(raw)
			if err != nil {
				return ""
			}
			return base.ResolveReference(next).String()
		}
	}
	return ""
}
