// Package ci is documented in doc.go.
package ci

// cspell:ignore commitish apihttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type apiStep struct {
	method, path, body, link string
	status                   int
}

func githubSequence(t *testing.T, steps []apiStep) *GitHubClient {
	t.Helper()
	index := 0
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer token" ||
				r.Header.Get("Accept") != githubAcceptV3 ||
				r.Header.Get("X-GitHub-Api-Version") != githubAPIVersion {
				t.Error("missing API headers")
			}
			if index >= len(steps) {
				t.Errorf("unexpected %s %s", r.Method, r.URL)
				w.WriteHeader(500)
				return
			}
			s := steps[index]
			index++
			if r.Method != s.method || r.URL.RequestURI() != s.path {
				t.Errorf(
					"step %d: got %s %s want %s %s",
					index,
					r.Method,
					r.URL.RequestURI(),
					s.method,
					s.path,
				)
			}
			if s.link != "" {
				w.Header().Set("Link", s.link)
			}
			if s.status == 0 {
				s.status = 200
			}
			w.WriteHeader(s.status)
			_, _ = io.WriteString(w, s.body)
		}),
	)
	t.Cleanup(func() {
		server.Close()
		if index != len(steps) {
			t.Errorf("called %d of %d steps", index, len(steps))
		}
	})
	c, err := NewGitHubClient(
		WithGitHubRepo("owner/repo"),
		WithGitHubToken("token"),
		WithGitHubAPIBaseURL(
			server.URL,
		),
		WithGitHubUploadBaseURL(server.URL),
		WithGitHubHTTPClient(server.Client()),
		WithGitHubBackoff(
			BackoffOptions{MaxRetriesTime: -1},
		),
		WithGitHubNoProgressBar(true),
		WithGitHubStderr(io.Discard),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const releasePath = "/repos/owner/repo/releases"

func TestNewGitHubClient(t *testing.T) {
	for _, opts := range [][]GitHubOption{nil, {WithGitHubRepo("owner/repo")}} {
		if _, err := NewGitHubClient(opts...); !errors.Is(err, ErrMissingOption) {
			t.Fatal(err)
		}
	}
	for _, repo := range []string{"owner", "o/r/x", "/r", "o/"} {
		if _, err := NewGitHubClient(WithGitHubRepo(repo), WithGitHubToken("t")); err == nil {
			t.Fatal(repo)
		}
	}
	c, err := NewGitHubClient(
		WithGitHubRepo("o/r"),
		WithGitHubToken("t"),
		WithGitHubHTTPClient(nil),
		WithGitHubStderr(nil),
	)
	if err != nil || c.api != defaultAPIBaseURL ||
		c.upload != "https://uploads.github.com" ||
		c.http == nil ||
		c.stderr == nil {
		t.Fatalf("%+v %v", c, err)
	}
	if tr, ok := c.http.Transport.(*http.Transport); !ok ||
		tr.ResponseHeaderTimeout != defaultConnectTimeout {
		t.Fatalf("default HTTP client has no timeout: %+v", c.http.Transport)
	}
	if got := (&GitHubAPIError{400, "bad"}).Error(); got != "GitHub API 400: bad" {
		t.Fatal(got)
	}
}
func TestGitHubClient_FindRelease(t *testing.T) {
	ctx := context.Background()
	c := githubSequence(t, []apiStep{
		{
			method: "GET",
			path:   releasePath + "/tags/app-v1",
			body:   `{"id":5,"tag_name":"app-v1"}`,
		},
		{method: "GET", path: releasePath + "/tags/missing", body: `{}`, status: 404},
		{
			method: "GET",
			path:   releasePath + "?per_page=100",
			body: `[{"id":9,"draft":true,"tag_name":"v999"},{"id"` +
				`:1,"draft":false,"tag_name":"v999"}]`,
			link: `<` + releasePath + `?page=2>; rel="next", <other>; rel="last"`,
		},
		{
			method: "GET",
			path:   releasePath + "?page=2",
			body: `[{"id":8,"draft":true,"tag_name":"other"},{"id` +
				`":3,"draft":true,"tag_name":"v999"},{"id":7,"d` +
				`raft":true,"tag_name":"v999"}]`,
		},
		{method: "GET", path: releasePath + "?per_page=100", body: `[]`},
	})
	if _, err := c.FindRelease(ctx); !errors.Is(err, ErrMissingOption) {
		t.Fatal(err)
	}
	r, err := c.FindRelease(ctx, WithReleaseTag("app-v1"))
	if err != nil || r.ID != 5 {
		t.Fatalf("%v %v", r, err)
	}
	r, err = c.FindRelease(ctx, WithReleaseTag("missing"))
	if err != nil || r != nil {
		t.Fatalf("%v %v", r, err)
	}
	r, err = c.FindRelease(ctx, WithReleaseTag("v999"), WithReleaseDraft(true))
	if err != nil || r.ID != 3 {
		t.Fatalf("%v %v", r, err)
	}
	r, err = c.FindRelease(ctx, WithReleaseTag("none"), WithReleaseDraft(true))
	if err != nil || r != nil {
		t.Fatalf("%v %v", r, err)
	}
	for _, draft := range []bool{false, true} {
		t.Run(fmt.Sprint(draft), func(t *testing.T) {
			path := releasePath + "/tags/x"
			if draft {
				path = releasePath + "?per_page=100"
			}
			c := githubSequence(t, []apiStep{{method: "GET", path: path, body: "broken"}})
			if _, err := c.FindRelease(
				ctx,
				WithReleaseTag("x"),
				WithReleaseDraft(draft),
			); err == nil {
				t.Fatal("decode error")
			}
		})
	}
}
func TestGitHubClient_EnsureRelease(t *testing.T) {
	ctx := context.Background()
	get := func(body string, code int) apiStep {
		return apiStep{method: "GET", path: releasePath + "/tags/x", body: body, status: code}
	}
	post := func(body string, code int) apiStep {
		return apiStep{method: "POST", path: releasePath, body: body, status: code}
	}
	patch := func(code int) apiStep {
		return apiStep{
			method: "PATCH",
			path:   releasePath + "/4",
			body:   `{"id":4,"name":"new","body":"notes"}`,
			status: code,
		}
	}
	for _, tt := range []struct {
		name  string
		steps []apiStep
		fail  bool
	}{
		{"unchanged", []apiStep{get(`{"id":4,"name":"new","body":"notes"}`, 200)}, false},
		{"update", []apiStep{get(`{"id":4}`, 200), patch(200)}, false},
		{"update failure", []apiStep{get(`{"id":4}`, 200), patch(400)}, true},
		{"find failure", []apiStep{get(`{}`, 400)}, true},
		{"create", []apiStep{get(`{}`, 404), post(`{"id":4,"tag_name":"x"}`, 201)}, false},
		{"create failure", []apiStep{get(`{}`, 404), post(`{}`, 400)}, true},
		{
			"create validation",
			[]apiStep{get(`{}`, 404), post(`{"message":"validation"}`, 422)},
			true,
		},
		{
			"race",
			[]apiStep{
				get(`{}`, 404),
				post(`{"errors":[{"code":"already_exists"}]}`, 422),
				get(`{"id":4}`, 200),
				patch(200),
			},
			false,
		},
		{
			"race missing",
			[]apiStep{get(`{}`, 404), post(`already_exists`, 422), get(`{}`, 404)},
			true,
		},
		{
			"race find failure",
			[]apiStep{get(`{}`, 404), post(`already_exists`, 422), get(`{}`, 400)},
			true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := githubSequence(t, tt.steps)
			r, err := c.EnsureRelease(
				ctx,
				WithReleaseTag("x"),
				WithReleaseName("new"),
				WithReleaseBody("notes"),
				WithReleaseTarget("master"),
			)
			if (err != nil) != tt.fail {
				t.Fatalf("%v %v", r, err)
			}
			if !tt.fail && (r == nil || r.ID != 4) {
				t.Fatal(r)
			}
		})
	}
}
func TestGitHubClient_updateRelease(t *testing.T) {
	tests := []struct {
		name               string
		opts               []ReleaseOption
		wantName, wantBody string
		payload            string
	}{
		{name: "omitted", wantName: "old", wantBody: "notes"},
		{name: "unchanged name", opts: []ReleaseOption{WithReleaseName("old")},
			wantName: "old", wantBody: "notes"},
		{name: "unchanged body", opts: []ReleaseOption{WithReleaseBody("notes")},
			wantName: "old", wantBody: "notes"},
		{name: "name only", opts: []ReleaseOption{WithReleaseName("new")},
			wantName: "new", wantBody: "notes", payload: `{"name":"new"}`},
		{name: "body only", opts: []ReleaseOption{WithReleaseBody("new notes")},
			wantName: "old", wantBody: "new notes", payload: `{"body":"new notes"}`},
		{name: "clear name", opts: []ReleaseOption{WithReleaseName("")},
			wantBody: "notes", payload: `{"name":""}`},
		{name: "clear body", opts: []ReleaseOption{WithReleaseBody("")},
			wantName: "old", payload: `{"body":""}`},
		{name: "clear both", opts: []ReleaseOption{WithReleaseName(""), WithReleaseBody("")},
			payload: `{"body":"","name":""}`},
		{name: "change both", opts: []ReleaseOption{
			WithReleaseName("new"), WithReleaseBody("new notes")},
			wantName: "new", wantBody: "new notes",
			payload: `{"body":"new notes","name":"new"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			want := Release{ID: 4, Name: tt.wantName, Body: tt.wantBody}
			c, err := NewGitHubClient(WithGitHubRepo("owner/repo"), WithGitHubToken("token"),
				WithGitHubHTTPClient(&http.Client{Transport: testRoundTrip(
					func(r *http.Request) (*http.Response, error) {
						calls++
						data, err := io.ReadAll(r.Body)
						if err != nil || string(data) != tt.payload ||
							r.Method != http.MethodPatch || r.URL.Path != releasePath+"/4" {
							t.Errorf("PATCH payload %q, method %s, path %s, error %v",
								data, r.Method, r.URL.Path, err)
						}
						body, err := json.Marshal(want)
						if err != nil {
							t.Fatal(err)
						}
						return &http.Response{StatusCode: 200, Header: http.Header{},
							Body: io.NopCloser(bytes.NewReader(body))}, nil
					})}), WithGitHubStderr(io.Discard))
			if err != nil {
				t.Fatal(err)
			}
			original := &Release{ID: 4, Name: "old", Body: "notes"}
			got, err := c.updateRelease(context.Background(), original, releaseOptions(tt.opts))
			if err != nil || got == nil || got.ID != want.ID ||
				got.Name != want.Name || got.Body != want.Body {
				t.Fatalf("updateRelease() = %+v, error %v, want %+v", got, err, want)
			}
			wantCalls := 1
			if tt.payload == "" {
				wantCalls = 0
				if got != original {
					t.Error("unchanged release was not returned")
				}
			}
			if calls != wantCalls {
				t.Fatalf("PATCH calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}
func TestGitHubClient_EnsureDraftRace(t *testing.T) {
	ctx := context.Background()
	list := func(body string, code int) apiStep {
		return apiStep{
			method: "GET",
			path:   releasePath + "?per_page=100",
			body:   body,
			status: code,
		}
	}
	post := apiStep{
		method: "POST",
		path:   releasePath,
		body:   `{"id":9,"tag_name":"v999","draft":true}`,
	}
	for _, tt := range []struct {
		name  string
		steps []apiStep
		id    int64
		fail  bool
	}{
		{"lowest existing", []apiStep{list(`[{"id":4,"tag_name":"v999","draft":true,"name"`+
			`:"old"}]`, 200)}, 4, false},
		{"converge", []apiStep{list(`[]`, 200), post, list(`[{"id":3,"tag_name":"v999","dr`+
			`aft":true},{"id":9,"tag_name":"v999","draft":t`+
			`rue}]`, 200), {
			method: "DELETE",
			path:   releasePath + "/9",
			status: 204,
		}}, 3, false},
		{"alone", []apiStep{list(`[]`, 200), post, list(`[{"id":9,"tag_name":"v999","draft`+
			`":true}]`, 200)}, 9, false},
		{"not yet visible", []apiStep{list(`[]`, 200), post, list(`[]`, 200)}, 9, false},
		{"post-list fails", []apiStep{list(`[]`, 200), post, list(`{}`, 400)}, 0, true},
		{"delete fails", []apiStep{list(`[]`, 200), post, list(`[{"id":3,"tag_name":"v999"`+
			`,"draft":true}]`, 200), {
			method: "DELETE",
			path:   releasePath + "/9",
			status: 400,
		}}, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := githubSequence(t, tt.steps)
			r, err := c.EnsureRelease(ctx, WithReleaseTag("v999"), WithReleaseDraft(true))
			if (err != nil) != tt.fail || (!tt.fail && r.ID != tt.id) {
				t.Fatalf("%v %v", r, err)
			}
		})
	}
}
func TestGitHubClient_UploadAsset(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := githubSequence(t, []apiStep{
		{
			method: "GET",
			path:   releasePath + "/5/assets?per_page=100",
			body:   `[{"id":1,"name":"asset.bin"},{"id":2,"name":"keep"}]`,
			link:   `<` + releasePath + `/5/assets?page=2>; rel="next"`,
		},
		{
			method: "GET",
			path:   releasePath + "/5/assets?page=2",
			body:   `[{"id":3,"name":"asset.bin"}]`,
		},
		{method: "DELETE", path: releasePath + "/assets/1", status: 204},
		{method: "DELETE", path: releasePath + "/assets/3", status: 404},
		{
			method: "POST",
			path:   releasePath + "/5/assets?name=asset.bin",
			body:   `{"id":7,"name":"asset.bin"}`,
		},
		{method: "POST", path: releasePath + "/5/assets?name=payload", body: `{"id":8}`},
	})
	asset, err := c.UploadAsset(
		ctx,
		WithAssetRelease(5),
		WithAssetPath(file),
		WithAssetName("asset.bin"),
		WithAssetReplace(true),
	)
	if err != nil || asset.ID != 7 {
		t.Fatalf("%v %v", asset, err)
	}
	if _, err := c.UploadAsset(ctx, WithAssetRelease(5), WithAssetPath(file)); err != nil {
		t.Fatal(err)
	}
	for _, opts := range [][]AssetOption{
		nil,
		{WithAssetRelease(1)},
		{WithAssetRelease(1), WithAssetPath("/missing/file")},
		{WithAssetRelease(1), WithAssetPath(t.TempDir())},
	} {
		if _, err := c.UploadAsset(ctx, opts...); err == nil {
			t.Fatal("validation failure")
		}
	}
	if _, err := c.ListAssets(ctx); !errors.Is(err, ErrMissingOption) {
		t.Fatal(err)
	}
	if err := c.DeleteAsset(ctx); !errors.Is(err, ErrMissingOption) {
		t.Fatal(err)
	}
	if err := c.DeleteRelease(ctx); !errors.Is(err, ErrMissingOption) {
		t.Fatal(err)
	}
}

const assetConflictResponse = `{"message":"Validation Failed","errors":[` +
	`{"resource":"ReleaseAsset","code":"already_exists","field":"name"}]}`

func TestGitHubClient_UploadAssetConflict(t *testing.T) {
	file := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	list := func(body string) apiStep {
		return apiStep{method: "GET", path: releasePath + "/1/assets?per_page=100",
			body: body}
	}
	post := func(body string, status int) apiStep {
		return apiStep{method: "POST", path: releasePath + "/1/assets?name=asset.bin",
			body: body, status: status}
	}
	remove := func(status int) apiStep {
		return apiStep{method: "DELETE", path: releasePath + "/assets/2", status: status}
	}
	matching := `[{"id":2,"name":"asset.bin"},{"id":3,"name":"keep"}]`
	success := `{"id":7,"name":"asset.bin"}`
	last := strings.ReplaceAll(assetConflictResponse, "Validation Failed", "Last failure")
	for _, tt := range []struct {
		name    string
		replace bool
		steps   []apiStep
		status  int
		message string
	}{
		{
			name: "conflict resolved", replace: true,
			steps: []apiStep{list(`[]`), post(assetConflictResponse, 422),
				list(matching), remove(204), post(success, 201)},
		},
		{
			name: "concurrent deletion returns 404", replace: true,
			steps: []apiStep{list(`[]`), post(assetConflictResponse, 422),
				list(matching), remove(404), post(success, 201)},
		},
		{
			name: "conflict already disappeared", replace: true,
			steps: []apiStep{list(`[]`), post(assetConflictResponse, 422),
				list(`[{"id":3,"name":"keep"}]`), post(success, 201)},
		},
		{
			name: "third attempt succeeds", replace: true,
			steps: []apiStep{list(`[]`), post(assetConflictResponse, 422),
				list(matching), remove(204), post(assetConflictResponse, 422),
				list(matching), remove(204), post(success, 201)},
		},
		{
			name: "three conflicts return last error", replace: true,
			steps: []apiStep{list(`[]`), post(assetConflictResponse, 422),
				list(matching), remove(204), post(assetConflictResponse, 422),
				list(matching), remove(204), post(last, 422)},
			status: 422, message: last,
		},
		{
			name:   "replace disabled",
			steps:  []apiStep{post(assetConflictResponse, 422)},
			status: 422, message: assetConflictResponse,
		},
		{
			name: "other validation failure", replace: true,
			steps:  []apiStep{list(`[]`), post(`{"message":"Validation Failed"}`, 422)},
			status: 422, message: `{"message":"Validation Failed"}`,
		},
		{
			name: "other status with duplicate diagnostic", replace: true,
			steps:  []apiStep{list(`[]`), post(assetConflictResponse, 409)},
			status: 409, message: assetConflictResponse,
		},
		{
			name: "relist fails", replace: true,
			steps: []apiStep{list(`[]`), post(assetConflictResponse, 422),
				{method: "GET", path: releasePath + "/1/assets?per_page=100",
					body: "list failed", status: 400}},
			status: 400, message: "list failed",
		},
		{
			name: "replacement deletion fails", replace: true,
			steps: []apiStep{list(`[]`), post(assetConflictResponse, 422),
				list(matching), {method: "DELETE", path: releasePath + "/assets/2",
					body: "delete failed", status: 400}},
			status: 400, message: "delete failed",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := githubSequence(t, tt.steps)
			got, err := c.UploadAsset(context.Background(), WithAssetRelease(1),
				WithAssetPath(file), WithAssetName("asset.bin"),
				WithAssetReplace(tt.replace))
			if tt.status == 0 {
				if err != nil || got == nil || got.ID != 7 || got.Name != "asset.bin" {
					t.Fatalf("UploadAsset() = %+v, error %v", got, err)
				}
				return
			}
			var api *GitHubAPIError
			if got != nil || !errors.As(err, &api) || api.StatusCode != tt.status ||
				api.Message != tt.message {
				t.Fatalf("UploadAsset() = %+v, error %v, want %d %q",
					got, err, tt.status, tt.message)
			}
		})
	}
}
func TestGitHubClient_UploadAssetReplaceRequestError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := githubSequence(t, []apiStep{{method: "GET",
		path: releasePath + "/1/assets?per_page=100", body: `[]`}})
	c.upload = "http://["
	got, err := c.UploadAsset(context.Background(), WithAssetRelease(1),
		WithAssetPath(file), WithAssetReplace(true))
	var api *GitHubAPIError
	if got != nil || err == nil || errors.As(err, &api) ||
		!strings.Contains(err.Error(), "create GitHub request") {
		t.Fatalf("UploadAsset() = %+v, error %v", got, err)
	}
}
func TestGitHubClient_UploadAssetDisconnect(t *testing.T) {
	file := filepath.Join(t.TempDir(), "asset.bin")
	content := "complete payload"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, replace := range []bool{false, true} {
		for _, truncated := range []bool{false, true} {
			t.Run(fmt.Sprintf("replace=%t/truncated=%t", replace, truncated),
				func(t *testing.T) {
					var lock sync.Mutex
					uploads := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
						r *http.Request) {
						if r.Method == http.MethodGet {
							_, _ = io.WriteString(w, `[]`)
							return
						}
						lock.Lock()
						uploads++
						call := uploads
						lock.Unlock()
						if call > 1 {
							w.WriteHeader(422)
							_, _ = io.WriteString(w, assetConflictResponse)
							return
						}
						data, err := io.ReadAll(r.Body)
						if r.Method != http.MethodPost || string(data) != content || err != nil ||
							r.ContentLength != int64(len(content)) ||
							r.Header.Get("Content-Type") != "application/octet-stream" {
							t.Errorf("upload %q, method %s, length %d, error %v", data,
								r.Method, r.ContentLength, err)
						}
						if truncated {
							w.Header().Set("Content-Length", "100")
							_, _ = io.WriteString(w, `{"id":`)
							return
						}
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
					}))
					t.Cleanup(server.Close)
					c, err := NewGitHubClient(WithGitHubRepo("owner/repo"),
						WithGitHubToken("token"), WithGitHubAPIBaseURL(server.URL),
						WithGitHubUploadBaseURL(server.URL), WithGitHubHTTPClient(server.Client()),
						WithGitHubBackoff(fastBackoff()), WithGitHubNoProgressBar(true),
						WithGitHubStderr(io.Discard))
					if err != nil {
						t.Fatal(err)
					}
					got, err := c.UploadAsset(context.Background(), WithAssetRelease(1),
						WithAssetPath(file), WithAssetReplace(replace))
					lock.Lock()
					calls := uploads
					lock.Unlock()
					if err == nil || got != nil || calls != 1 {
						t.Fatalf("UploadAsset() = %+v, error %v, uploads %d, want one",
							got, err, calls)
					}
					var api *GitHubAPIError
					if errors.As(err, &api) {
						t.Fatalf("ambiguous upload was retried: %v", err)
					}
				})
		}
	}
}
func TestGitHubClient_AssetFailures(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, steps := range [][]apiStep{
		{{method: "GET", path: releasePath + "/1/assets?per_page=100", body: "invalid"}},
		{
			{
				method: "GET",
				path:   releasePath + "/1/assets?per_page=100",
				body:   `[{"id":2,"name":"file"}]`,
			},
			{method: "DELETE", path: releasePath + "/assets/2", status: 400},
		},
		{
			{method: "GET", path: releasePath + "/1/assets?per_page=100", body: `[]`},
			{method: "POST", path: releasePath + "/1/assets?name=file", status: 400},
		},
	} {
		c := githubSequence(t, steps)
		if _, err := c.UploadAsset(
			ctx,
			WithAssetRelease(1),
			WithAssetPath(file),
			WithAssetReplace(true),
		); err == nil {
			t.Fatal("expected failure")
		}
	}
	c := githubSequence(
		t,
		[]apiStep{
			{method: "DELETE", path: releasePath + "/1", status: 404},
			{method: "DELETE", path: releasePath + "/2", status: 204},
			{method: "DELETE", path: releasePath + "/3", status: 400},
		},
	)
	if err := c.DeleteRelease(ctx, WithReleaseID(1)); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRelease(ctx, WithReleaseID(2)); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRelease(ctx, WithReleaseID(3)); err == nil {
		t.Fatal("delete error")
	}
}
func fastBackoff() BackoffOptions {
	return BackoffOptions{
		MaxRetriesTime: 10 * time.Millisecond,
		BackoffInitial: time.Millisecond,
		BackoffStep:    time.Millisecond,
		BackoffCap:     time.Millisecond,
	}
}

type testRoundTrip func(*http.Request) (*http.Response, error)

func (f testRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGitHubClient_request(t *testing.T) {
	ctx := context.Background()
	var pauses []time.Duration
	var pauseErr error
	original := githubPause
	githubPause = func(_ context.Context, delay time.Duration) error {
		pauses = append(pauses, delay)
		return pauseErr
	}
	t.Cleanup(func() { githubPause = original })
	for _, tt := range []struct {
		code  int
		rate  bool
		calls int
	}{{
		500,
		false,
		2,
	}, {
		429,
		false,
		2,
	}, {
		408,
		false,
		2,
	}, {
		403,
		true,
		2,
	}, {
		403,
		false,
		1,
	}, {
		400,
		false,
		1,
	}} {
		t.Run(fmt.Sprintf("%d-%t", tt.code, tt.rate), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					if calls == 1 {
						if tt.rate {
							w.Header().Set("X-RateLimit-Remaining", "0")
						}
						w.WriteHeader(tt.code)
						_, _ = io.WriteString(w, `{"message":"failure"}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":1}`)
				}),
			)
			defer server.Close()
			c, _ := NewGitHubClient(
				WithGitHubRepo("o/r"),
				WithGitHubToken("t"),
				WithGitHubAPIBaseURL(server.URL),
				WithGitHubBackoff(fastBackoff()),
				WithGitHubStderr(io.Discard),
			)
			_, err := c.FindRelease(ctx, WithReleaseTag("x"))
			if calls != tt.calls || (err != nil) != (tt.calls == 1) {
				t.Fatalf("calls %d err %v", calls, err)
			}
		})
	}
	c, _ := NewGitHubClient(
		WithGitHubRepo("o/r"),
		WithGitHubToken("t"),
		WithGitHubBackoff(BackoffOptions{MaxRetriesTime: -1}),
		WithGitHubStderr(io.Discard),
	)
	for _, tt := range []struct {
		name      string
		transport testRoundTrip
	}{
		{
			"network",
			func(*http.Request) (*http.Response, error) {
				return nil, errors.New(
					"connection reset",
				)
			},
		},
		{"error body", func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 500,
				Header:     http.Header{},
				Body:       io.NopCloser(errorReader{}),
			}, nil
		}},
		{"success body", func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{},
				Body:       io.NopCloser(errorReader{}),
			}, nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c.http = &http.Client{Transport: tt.transport}
			if _, err := c.FindRelease(ctx, WithReleaseTag("x")); err == nil {
				t.Fatal("failure")
			}
		})
	}
	if _, err := c.request(ctx, "GET", "http://[", nil, nil, "", 0); err == nil {
		t.Fatal("bad URL")
	}
	t.Run("stalled body", func(t *testing.T) {
		original := responseStallTimeout
		responseStallTimeout = 50 * time.Millisecond
		t.Cleanup(func() { responseStallTimeout = original })
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
			r *http.Request) {
			calls++
			if calls > 1 {
				_, _ = io.WriteString(w, `{"id":1}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":`)
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer server.Close()
		var diagnostic bytes.Buffer
		c, _ := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("t"),
			WithGitHubAPIBaseURL(server.URL), WithGitHubBackoff(fastBackoff()),
			WithGitHubStderr(&diagnostic))
		rel, err := c.FindRelease(ctx, WithReleaseTag("x"))
		if err != nil || rel == nil || rel.ID != 1 || calls != 2 ||
			!strings.Contains(diagnostic.String(), "response stalled for 50ms") {
			t.Fatalf("release %+v, calls %d, err %v, diagnostic %s", rel, calls, err,
				&diagnostic)
		}
	})
	t.Run("slow body", func(t *testing.T) {
		original := responseReadTimeout
		responseReadTimeout = 100 * time.Millisecond
		t.Cleanup(func() { responseReadTimeout = original })
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
			r *http.Request) {
			calls++
			if calls > 1 {
				_, _ = io.WriteString(w, `{"id":1}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":`)
			for {
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
					_, _ = io.WriteString(w, " ")
				}
			}
		}))
		defer server.Close()
		var diagnostic bytes.Buffer
		c, _ := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("t"),
			WithGitHubAPIBaseURL(server.URL), WithGitHubBackoff(fastBackoff()),
			WithGitHubStderr(&diagnostic))
		rel, err := c.FindRelease(ctx, WithReleaseTag("x"))
		if err != nil || rel == nil || rel.ID != 1 || calls != 2 ||
			!strings.Contains(diagnostic.String(), "response read exceeded 100ms") {
			t.Fatalf("release %+v, calls %d, err %v, diagnostic %s", rel, calls, err,
				&diagnostic)
		}
	})
	response := func(code int, body io.Reader) testRoundTrip {
		return func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Header: http.Header{},
				Body: io.NopCloser(body)}, nil
		}
	}
	for _, tt := range []struct {
		name  string
		first testRoundTrip
		posts int
	}{
		{"server error", response(500, strings.NewReader("failure")), 1},
		{"timeout", response(408, strings.NewReader("failure")), 1},
		{"rate limited", response(429, strings.NewReader("failure")), 2},
		{"unreadable error", response(500, errorReader{}), 1},
		{"unreadable success", response(200, errorReader{}), 1},
		{"reset", func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("reset")}
		}, 1},
		{"dial", func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}
		}, 2},
	} {
		t.Run("create "+tt.name, func(t *testing.T) {
			posts := 0
			c, _ := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("t"),
				WithGitHubBackoff(fastBackoff()), WithGitHubStderr(io.Discard),
				WithGitHubHTTPClient(&http.Client{Transport: testRoundTrip(
					func(r *http.Request) (*http.Response, error) {
						if r.Method == http.MethodGet {
							return response(200, strings.NewReader("[]"))(r)
						}
						posts++
						if posts == 1 {
							return tt.first(r)
						}
						return response(201, strings.NewReader(`{"id":1}`))(r)
					})}))
			rel, err := c.EnsureRelease(ctx, WithReleaseTag("x"), WithReleaseDraft(true))
			if posts != tt.posts || (err != nil) != (tt.posts == 1) ||
				(err == nil && rel.ID != 1) {
				t.Fatalf("posts %d release %+v err %v", posts, rel, err)
			}
		})
	}
	if _, err := c.request(
		ctx,
		"POST",
		"http://example.test",
		nil,
		nil,
		"/missing/file",
		0,
	); err == nil {
		t.Fatal("open failure")
	}
	for _, tt := range []struct{ link, current, want string }{
		{
			"nonsense",
			"http://example.test/a",
			"",
		}, {
			`<x>; rel="prev"`,
			"http://example.test/a",
			"",
		},
		{
			`<next>; type="json"; rel="next"`,
			"http://example.test/a",
			"http://example.test/next",
		},
		{
			`<http://[>; rel="next"`,
			"http://example.test",
			"",
		}, {
			`<x>; rel="next"`,
			"http://[",
			"",
		},
	} {
		if got := nextPage(tt.link, tt.current); got != tt.want {
			t.Fatalf("nextPage %q", got)
		}
	}
	for _, tt := range []struct {
		name, after string
		code, calls int
		pauseErr    error
		overBudget  bool
	}{
		{name: "secondary 403", after: "60", code: 403, calls: 2},
		{name: "secondary 429", after: "60", code: 429, calls: 2},
		{name: "pause canceled", after: "60", code: 403, calls: 1,
			pauseErr: context.Canceled},
		{name: "pause over budget", after: "3600", code: 403, calls: 1,
			overBudget: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pauses, pauseErr = nil, tt.pauseErr
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
				_ *http.Request) {
				calls++
				if calls == 1 {
					w.Header().Set("Retry-After", tt.after)
					w.Header().Set("X-RateLimit-Remaining", "5")
					w.WriteHeader(tt.code)
					_, _ = io.WriteString(w, `{"message":"secondary rate limit"}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":1}`)
			}))
			defer server.Close()
			backoff := fastBackoff()
			if !tt.overBudget {
				backoff.MaxRetriesTime = 2 * time.Minute
			}
			c, _ := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("t"),
				WithGitHubAPIBaseURL(server.URL), WithGitHubBackoff(backoff),
				WithGitHubStderr(io.Discard))
			_, err := c.FindRelease(ctx, WithReleaseTag("x"))
			if tt.overBudget {
				if !isGitHubStatus(err, tt.code) || calls != 1 || len(pauses) != 0 {
					t.Fatalf("calls %d err %v pauses %v", calls, err, pauses)
				}
				return
			}
			if calls != tt.calls || !errors.Is(err, tt.pauseErr) ||
				len(pauses) != 1 || pauses[0] != time.Minute {
				t.Fatalf("calls %d err %v pauses %v", calls, err, pauses)
			}
		})
	}
}

type releaseTestProgress struct{ err error }

func (p *releaseTestProgress) WrapReader(r io.Reader) io.ReadCloser {
	return io.NopCloser(r)
}
func (p *releaseTestProgress) WrapWriter(w io.Writer) io.WriteCloser {
	return nopWriteCloser{w}
}
func (p *releaseTestProgress) AddBytes(int64)   {}
func (p *releaseTestProgress) SetTotal(int64)   {}
func (p *releaseTestProgress) Finish(err error) { p.err = err }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
func TestGitHubClientUploadProgressFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "asset")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	bar := &releaseTestProgress{}
	original := releaseProgressBar
	releaseProgressBar = func(context.Context, ProgressOptions) ProgressBar { return bar }
	t.Cleanup(func() { releaseProgressBar = original })
	c, err := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("token"),
		WithGitHubBackoff(BackoffOptions{MaxRetriesTime: -1}),
		WithGitHubHTTPClient(&http.Client{Transport: testRoundTrip(
			func(*http.Request) (*http.Response, error) {
				return nil, errors.New("upload transport failed")
			})}), WithGitHubStderr(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UploadAsset(context.Background(), WithAssetRelease(1), WithAssetPath(file))
	if err == nil || bar.err == nil || bar.err.Error() != err.Error() {
		t.Fatalf("progress reported success: returned %v, progress %v", err, bar.err)
	}
}
func TestGitHubClientEmptyAsset(t *testing.T) {
	file := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		if r.ContentLength != 0 {
			t.Errorf("empty asset length: %d", r.ContentLength)
		}
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	defer server.Close()
	c, _ := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("token"),
		WithGitHubUploadBaseURL(server.URL), WithGitHubNoProgressBar(true))
	if _, err := c.UploadAsset(context.Background(), WithAssetRelease(1),
		WithAssetPath(file)); err != nil {
		t.Fatal(err)
	}
}
func TestGitHubClientDisconnect(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {
		calls++
		if calls == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	defer server.Close()
	c, _ := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("token"),
		WithGitHubAPIBaseURL(server.URL), WithGitHubBackoff(fastBackoff()),
		WithGitHubStderr(io.Discard))
	if _, err := c.FindRelease(context.Background(), WithReleaseTag("x")); err != nil ||
		calls != 2 {
		t.Fatalf("calls %d: %v", calls, err)
	}
}
func TestGitHubClientConcurrentDraft(t *testing.T) {
	var lock sync.Mutex
	var created sync.WaitGroup
	created.Add(2)
	var releases []Release
	var deleted []int64
	initialLists := 0
	initialReady := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		switch r.Method {
		case "GET":
			lock.Lock()
			if initialLists < 2 {
				initialLists++
				if initialLists == 2 {
					close(initialReady)
				}
				lock.Unlock()
				<-initialReady
				_, _ = io.WriteString(w, `[]`)
				return
			}
			page := append([]Release(nil), releases...)
			lock.Unlock()
			_ = json.NewEncoder(w).Encode(page)
		case "POST":
			var payload struct {
				Tag   string `json:"tag_name"`
				Draft bool   `json:"draft"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Tag != "v999" || !payload.Draft {
				t.Error("draft payload")
			}
			lock.Lock()
			rel := Release{ID: int64(len(releases) + 1), TagName: "v999", Draft: true}
			releases = append(releases, rel)
			lock.Unlock()
			created.Done()
			created.Wait()
			_ = json.NewEncoder(w).Encode(rel)
		case "DELETE":
			lock.Lock()
			deleted = append(deleted, 2)
			lock.Unlock()
			w.WriteHeader(204)
		default:
			t.Error("unexpected method")
		}
	}))
	defer server.Close()
	c, _ := NewGitHubClient(
		WithGitHubRepo("o/r"),
		WithGitHubToken("token"),
		WithGitHubAPIBaseURL(
			server.URL,
		),
		WithGitHubBackoff(BackoffOptions{MaxRetriesTime: -1}),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var callers sync.WaitGroup
	callers.Add(2)
	for range 2 {
		go func() {
			defer callers.Done()
			rel, err := c.EnsureRelease(ctx, WithReleaseTag("v999"), WithReleaseDraft(true))
			if err != nil || rel.ID != 1 {
				t.Errorf("draft convergence: %+v %v", rel, err)
			}
		}()
	}
	callers.Wait()
	if len(deleted) != 1 {
		t.Fatalf("deleted releases: %v", deleted)
	}
}
func TestGitHubClientCreatePayload(t *testing.T) {
	tests := []struct {
		name    string
		opts    []ReleaseOption
		payload string
	}{
		{name: "all", opts: []ReleaseOption{WithReleaseName("title"),
			WithReleaseBody("notes"), WithReleaseTarget("master")},
			payload: `{"body":"notes","draft":false,"name":"title","tag_name":"app-v1",` +
				`"target_commitish":"master"}`},
		{name: "omitted", payload: `{"draft":false,"tag_name":"app-v1"}`},
		{name: "empty", opts: []ReleaseOption{WithReleaseName(""), WithReleaseBody("")},
			payload: `{"body":"","draft":false,"name":"","tag_name":"app-v1"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
				r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(404)
					return
				}
				if data, _ := io.ReadAll(r.Body); string(data) != tt.payload {
					t.Errorf("POST payload %s, want %s", data, tt.payload)
				}
				_, _ = io.WriteString(w, `{"id":1}`)
			}))
			defer server.Close()
			c, _ := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("token"),
				WithGitHubAPIBaseURL(server.URL))
			opts := append([]ReleaseOption{WithReleaseTag("app-v1")}, tt.opts...)
			if _, err := c.EnsureRelease(context.Background(), opts...); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestGitHubClient_UploadRetry(t *testing.T) {
	file := filepath.Join(t.TempDir(), "asset.bin")
	content := "complete payload"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name      string
		code      int
		truncated bool
		wantErr   bool
		calls     int
	}{
		{name: "rate limited", code: 429, calls: 2},
		{name: "rate exhausted", code: 403, calls: 2},
		{name: "timeout", code: 408, wantErr: true, calls: 1},
		{name: "server error", code: 500, wantErr: true, calls: 1},
		{name: "bad gateway", code: 502, wantErr: true, calls: 1},
		{name: "unavailable", code: 503, wantErr: true, calls: 1},
		{name: "gateway timeout", code: 504, wantErr: true, calls: 1},
		{name: "unreadable error", code: 500, truncated: true, wantErr: true, calls: 1},
	} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replace=%t", tt.name, replace), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
					r *http.Request) {
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, `[]`)
						return
					}
					calls++
					data, err := io.ReadAll(r.Body)
					if err != nil || string(data) != content ||
						r.Method != http.MethodPost ||
						r.ContentLength != int64(len(content)) ||
						r.Header.Get("Content-Type") != "application/octet-stream" {
						t.Errorf("upload %d %q len %d err %v", calls, data, r.ContentLength, err)
					}
					if calls == 1 {
						if tt.code == 403 {
							w.Header().Set("X-RateLimit-Remaining", "0")
						}
						if tt.truncated {
							w.Header().Set("Content-Length", "100")
						}
						w.WriteHeader(tt.code)
						_, _ = io.WriteString(w, "upload response failed")
						return
					}
					if tt.wantErr {
						w.WriteHeader(422)
						_, _ = io.WriteString(w, assetConflictResponse)
						return
					}
					_, _ = io.WriteString(w, `{"id":1}`)
				}))
				t.Cleanup(server.Close)
				c, err := NewGitHubClient(WithGitHubRepo("o/r"), WithGitHubToken("t"),
					WithGitHubAPIBaseURL(server.URL), WithGitHubUploadBaseURL(server.URL),
					WithGitHubBackoff(fastBackoff()), WithGitHubStderr(io.Discard),
					WithGitHubNoProgressBar(true))
				if err != nil {
					t.Fatal(err)
				}
				got, err := c.UploadAsset(context.Background(), WithAssetRelease(1),
					WithAssetPath(file), WithAssetReplace(replace))
				if (err != nil) != tt.wantErr || calls != tt.calls {
					t.Fatalf("UploadAsset() = %+v, error %v, calls %d, want %d",
						got, err, calls, tt.calls)
				}
				if !tt.wantErr {
					if got == nil || got.ID != 1 {
						t.Fatalf("uploaded asset = %+v, want ID 1", got)
					}
					return
				}
				if got != nil {
					t.Fatalf("ambiguous upload returned asset %+v", got)
				}
				var api *GitHubAPIError
				if tt.truncated {
					if !strings.Contains(err.Error(), "read GitHub error") {
						t.Fatalf("unreadable response error = %v", err)
					}
				} else if !errors.As(err, &api) || api.StatusCode != tt.code {
					t.Fatalf("upload error = %v, want status %d", err, tt.code)
				}
			})
		}
	}
}
func Test_githubRateLimit(t *testing.T) {
	reset := time.Now().Add(time.Hour).Unix()
	type args struct {
		h http.Header
	}
	tests := []struct {
		name  string
		args  args
		want  bool
		want1 time.Duration
	}{
		{name: "not limited", args: args{http.Header{"X-Ratelimit-Remaining": {"5"}}}},
		{name: "secondary", args: args{http.Header{"Retry-After": {"60"},
			"X-Ratelimit-Remaining": {"5"}}}, want: true, want1: time.Minute},
		{name: "unparsable retry after", args: args{http.Header{"Retry-After": {"soon"}}},
			want: true},
		{name: "exhausted", args: args{http.Header{"X-Ratelimit-Remaining": {"0"},
			"X-Ratelimit-Reset": {fmt.Sprint(reset)}}}, want: true,
			want1: time.Until(time.Unix(reset, 0))},
		{name: "exhausted without reset", args: args{http.Header{
			"X-Ratelimit-Remaining": {"0"}}}, want: true,
			want1: time.Until(time.Unix(0, 0))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1 := githubRateLimit(tt.args.h)
			if got != tt.want {
				t.Errorf("githubRateLimit() got = %v, want %v", got, tt.want)
			}
			// Reset-based waits shrink while the test runs.
			if got1 > tt.want1 || got1 < tt.want1-time.Minute {
				t.Errorf("githubRateLimit() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}
func Test_retryAPI(t *testing.T) {
	failure := errors.New("failure")
	fast := BackoffOptions{MaxRetriesTime: 10 * time.Millisecond,
		BackoffInitial: time.Millisecond, BackoffStep: time.Millisecond,
		BackoffCap: time.Millisecond}
	tests := []struct {
		name     string
		opts     RetryOptions
		results  []error
		pauseErr error
		calls    int
		pauses   []time.Duration
		wantErr  error
	}{
		{name: "success", results: []error{nil}, calls: 1},
		{name: "stop", results: []error{&stopRetry{failure}}, calls: 1, wantErr: failure},
		{name: "pause and backoff exceed budget", opts: RetryOptions{
			BackoffOptions: BackoffOptions{MaxRetriesTime: 111 * time.Second,
				BackoffInitial: time.Minute, BackoffCap: time.Minute}},
			results: []error{&retryPause{failure, time.Minute}}, calls: 1, wantErr: failure},
		{name: "pause counted for later backoff", opts: RetryOptions{BackoffOptions: fast},
			results: []error{&retryPause{failure, 5 * time.Millisecond}, failure,
				&retryPause{failure, 5 * time.Millisecond}},
			calls: 3, pauses: []time.Duration{5 * time.Millisecond}, wantErr: failure},
		{name: "negative pause", opts: RetryOptions{BackoffOptions: fast},
			results: []error{&retryPause{failure, -time.Hour}, nil}, calls: 2},
		{name: "pause canceled", opts: RetryOptions{BackoffOptions: fast},
			results:  []error{&retryPause{failure, time.Millisecond}},
			pauseErr: context.Canceled, calls: 1,
			pauses: []time.Duration{time.Millisecond}, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pauses []time.Duration
			calls := 0
			tt.opts.Stderr = io.Discard
			err := retryAPI(context.Background(), tt.opts,
				func(_ context.Context, delay time.Duration) error {
					pauses = append(pauses, delay)
					return tt.pauseErr
				}, func(context.Context) error {
					calls++
					return tt.results[calls-1]
				})
			if !errors.Is(err, tt.wantErr) || (err == nil) != (tt.wantErr == nil) ||
				calls != tt.calls || fmt.Sprint(pauses) != fmt.Sprint(tt.pauses) {
				t.Errorf("retryAPI() error = %v, calls %d, pauses %v, want %v, %d, %v",
					err, calls, pauses, tt.wantErr, tt.calls, tt.pauses)
			}
		})
	}
}
func Test_newStallBody(t *testing.T) {
	originalStall, originalRead := responseStallTimeout, responseReadTimeout
	t.Cleanup(func() {
		responseStallTimeout, responseReadTimeout = originalStall, originalRead
	})
	type args struct {
		body         io.ReadCloser
		stall, limit time.Duration
	}
	tests := []struct {
		name                 string
		args                 args
		wantStalled, expired bool
	}{
		{name: "cancels silent body", args: args{io.NopCloser(strings.NewReader("")),
			time.Millisecond, time.Hour}, wantStalled: true},
		{name: "cancels slow body", args: args{io.NopCloser(strings.NewReader("")),
			time.Hour, time.Millisecond}, expired: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responseStallTimeout, responseReadTimeout = tt.args.stall, tt.args.limit
			canceled := make(chan struct{})
			got := newStallBody(tt.args.body, func() { close(canceled) })
			defer func() { _ = got.Close() }()
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("body was not canceled")
			}
			if got.ReadCloser != tt.args.body || got.timeout != tt.args.stall ||
				got.limit != tt.args.limit || got.stalled.Load() != tt.wantStalled ||
				got.expired.Load() != tt.expired {
				t.Errorf("newStallBody() timeout %v, limit %v, stalled %t, expired %t",
					got.timeout, got.limit, got.stalled.Load(), got.expired.Load())
			}
		})
	}
}
func Test_stallBody_Read(t *testing.T) {
	type fields struct {
		ReadCloser io.ReadCloser
		timeout    time.Duration
		stalled    bool
		expired    bool
	}
	type args struct {
		p []byte
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    int
		wantErr string
	}{
		{name: "data", fields: fields{ReadCloser: io.NopCloser(strings.NewReader("data")),
			timeout: time.Hour}, args: args{make([]byte, 8)}, want: 4},
		{name: "end of body", fields: fields{ReadCloser: io.NopCloser(strings.NewReader("")),
			timeout: time.Hour}, args: args{make([]byte, 8)}, wantErr: "EOF"},
		{name: "stalled", fields: fields{ReadCloser: io.NopCloser(errorReader{}),
			timeout: time.Second, stalled: true}, args: args{make([]byte, 8)},
			wantErr: "response stalled for 1s: read failure"},
		{name: "expired", fields: fields{ReadCloser: io.NopCloser(errorReader{}),
			timeout: time.Second, expired: true}, args: args{make([]byte, 8)},
			wantErr: "response read exceeded 1s: read failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &stallBody{
				ReadCloser: tt.fields.ReadCloser,
				timeout:    tt.fields.timeout,
				limit:      tt.fields.timeout,
				timer:      time.NewTimer(tt.fields.timeout),
			}
			defer b.timer.Stop()
			b.stalled.Store(tt.fields.stalled)
			b.expired.Store(tt.fields.expired)
			got, err := b.Read(tt.args.p)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("stallBody.Read() error = %v, wantErr %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("stallBody.Read() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("stallBody.Read() = %v, want %v", got, tt.want)
			}
		})
	}
}

type failingCloser struct{ io.Reader }

func (failingCloser) Close() error { return errors.New("close failure") }
func Test_stallBody_Close(t *testing.T) {
	type fields struct {
		ReadCloser io.ReadCloser
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
	}{
		{name: "stops timer", fields: fields{io.NopCloser(strings.NewReader(""))}},
		{name: "close failure", fields: fields{failingCloser{strings.NewReader("")}},
			wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &stallBody{
				ReadCloser: tt.fields.ReadCloser,
				timeout:    time.Hour,
				timer:      time.NewTimer(time.Hour),
				deadline:   time.NewTimer(time.Hour),
			}
			if err := b.Close(); (err != nil) != tt.wantErr {
				t.Errorf("stallBody.Close() error = %v, wantErr %v", err, tt.wantErr)
			}
			if b.timer.Stop() || b.deadline.Stop() {
				t.Error("stallBody.Close() left a timer running")
			}
		})
	}
}
func Test_newAPIHTTPClient(t *testing.T) {
	original := requestWriteTimeout
	requestWriteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { requestWriteTimeout = original })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)
	stalled, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stalled.Close() })
	go func() {
		// Accepted connections stay open and unread until the listener closes.
		var conns []net.Conn
		for {
			conn, err := stalled.Accept()
			if err != nil {
				for _, conn := range conns {
					_ = conn.Close()
				}
				return
			}
			conns = append(conns, conn)
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = closed.Close()
	tests := []struct {
		name    string
		url     string
		size    int
		want    string
		wantErr func(error) bool
	}{
		{name: "sends request", url: server.URL, size: 1 << 20, want: "ok"},
		{name: "peer stops reading", url: "http://" + stalled.Addr().String(),
			size: 64 << 20, wantErr: func(err error) bool {
				return errors.Is(err, os.ErrDeadlineExceeded)
			}},
		{name: "dial failure", url: "http://" + closed.Addr().String(),
			wantErr: requestUnsent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, tt.url,
				bytes.NewReader(make([]byte, tt.size)))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := newAPIHTTPClient().Do(req)
			if tt.wantErr != nil {
				if err == nil || !tt.wantErr(err) {
					t.Fatalf("newAPIHTTPClient().Do() error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("newAPIHTTPClient().Do() error = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if got, err := io.ReadAll(resp.Body); err != nil || string(got) != tt.want {
				t.Errorf("newAPIHTTPClient().Do() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}
func Test_writeDeadlineConn_Write(t *testing.T) {
	reading, peer := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	stalled, silent := net.Pipe()
	closed, _ := net.Pipe()
	_ = closed.Close()
	t.Cleanup(func() {
		for _, conn := range []net.Conn{reading, peer, stalled, silent} {
			_ = conn.Close()
		}
	})
	type fields struct {
		Conn    net.Conn
		timeout time.Duration
	}
	type args struct {
		p []byte
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    int
		wantErr error
	}{
		{name: "writes data", fields: fields{reading, time.Second},
			args: args{[]byte("data")}, want: 4},
		{name: "peer stops reading", fields: fields{stalled, 10 * time.Millisecond},
			args: args{[]byte("data")}, wantErr: os.ErrDeadlineExceeded},
		{name: "closed connection", fields: fields{closed, time.Second},
			args: args{[]byte("data")}, wantErr: io.ErrClosedPipe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &writeDeadlineConn{
				Conn:    tt.fields.Conn,
				timeout: tt.fields.timeout,
			}
			got, err := c.Write(tt.args.p)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("writeDeadlineConn.Write() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("writeDeadlineConn.Write() = %v, want %v", got, tt.want)
			}
		})
	}
}
func Test_getPages(t *testing.T) {
	type args struct {
		ctx   context.Context
		steps []apiStep
		next  string
	}
	ctx := context.Background()
	tests := []struct {
		name    string
		args    args
		want    []int
		wantErr bool
	}{
		{name: "pages", args: args{ctx, []apiStep{
			{method: "GET", path: releasePath + "?per_page=100", body: `[1]`,
				link: `<` + releasePath + `?page=2>; rel="next"`},
			{method: "GET", path: releasePath + "?page=2", body: `[2,3]`},
		}, "?per_page=100"}, want: []int{1, 2, 3}},
		{name: "empty", args: args{ctx, []apiStep{
			{method: "GET", path: releasePath + "?per_page=100", body: `[]`},
		}, "?per_page=100"}},
		{name: "request error", args: args{ctx, []apiStep{
			{method: "GET", path: releasePath + "?per_page=100", body: "broken"},
		}, "?per_page=100"}, wantErr: true},
		{name: "self loop", args: args{ctx, []apiStep{
			{method: "GET", path: releasePath + "?per_page=100", body: `[1]`,
				link: `<` + releasePath + `?per_page=100>; rel="next"`},
		}, "?per_page=100"}, wantErr: true},
		{name: "cycle", args: args{ctx, []apiStep{
			{method: "GET", path: releasePath + "?per_page=100", body: `[1]`,
				link: `<` + releasePath + `?page=2>; rel="next"`},
			{method: "GET", path: releasePath + "?page=2", body: `[2]`,
				link: `<` + releasePath + `?per_page=100>; rel="next"`},
		}, "?per_page=100"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := githubSequence(t, tt.args.steps)
			got, err := getPages[int](tt.args.ctx, c, c.endpoint(tt.args.next))
			if (err != nil) != tt.wantErr {
				t.Fatalf("getPages() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("getPages() = %v, want %v", got, tt.want)
			}
		})
	}
}
