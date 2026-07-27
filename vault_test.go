// Package ci is documented in doc.go.
// cspell:ignore hashicorp cleanhttp retryablehttp mapstructure hcl kubernetes nolint
package ci

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestNewVaultClient(t *testing.T) {
	type args struct {
		opts VaultOptions
	}
	tests := []struct {
		name    string
		args    args
		want    *VaultClient
		wantErr bool
	}{
		{
			name:    "missing address",
			args:    args{opts: VaultOptions{Token: "tok"}},
			wantErr: true,
		},
		{
			name: "missing token",
			args: args{opts: VaultOptions{
				Address: "http://vault:8200", // DevSkim: ignore DS137138
			}},
			wantErr: true,
		},
		{
			name:    "unparsable address",
			args:    args{opts: VaultOptions{Address: "://bad", Token: "tok"}},
			wantErr: true,
		},
		{
			name:    "relative address",
			args:    args{opts: VaultOptions{Address: "vault:8200", Token: "tok"}},
			wantErr: true,
		},
		{
			name: "valid options with default timeout",
			args: args{opts: VaultOptions{
				Address: "http://example.com:8200", // DevSkim: ignore DS137138
				Token:   "tok",
			}},
			want: &VaultClient{
				baseURL: mustParseURL(t,
					"http://example.com:8200"), // DevSkim: ignore DS137138
				token:      "tok",
				httpClient: &http.Client{Timeout: 30 * time.Second},
			},
		},
		{
			name: "valid options with custom timeout",
			args: args{opts: VaultOptions{
				Address:     "http://example.com:8200", // DevSkim: ignore DS137138
				Token:       "tok",
				HTTPTimeout: 5 * time.Second,
			}},
			want: &VaultClient{
				baseURL: mustParseURL(t,
					"http://example.com:8200"), // DevSkim: ignore DS137138
				token:      "tok",
				httpClient: &http.Client{Timeout: 5 * time.Second},
			},
		},
		{
			name: "custom HTTPClient overrides HTTPTimeout",
			args: args{opts: VaultOptions{
				Address:     "http://example.com:8200", // DevSkim: ignore DS137138
				Token:       "tok",
				HTTPTimeout: 5 * time.Second,
				HTTPClient:  &http.Client{Timeout: 7 * time.Second},
			}},
			want: &VaultClient{
				baseURL: mustParseURL(t,
					"http://example.com:8200"), // DevSkim: ignore DS137138
				token:      "tok",
				httpClient: &http.Client{Timeout: 7 * time.Second},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewVaultClient(tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewVaultClient() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NewVaultClient() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseVaultKVv2Path(t *testing.T) {
	type args struct {
		raw string
	}
	tests := []struct {
		name    string
		args    args
		want    VaultSecretRef
		wantErr bool
	}{
		{
			name:    "empty path",
			args:    args{raw: ""},
			wantErr: true,
		},
		{
			name:    "only slashes",
			args:    args{raw: "///"},
			wantErr: true,
		},
		{
			name:    "empty segment",
			args:    args{raw: "secret//key"},
			wantErr: true,
		},
		{
			name:    "single segment",
			args:    args{raw: "secret"},
			wantErr: true,
		},
		{
			name: "mount and key only",
			args: args{raw: "secret/key"},
			want: VaultSecretRef{Mount: "secret", Key: "key"},
		},
		{
			name: "mount path and key",
			args: args{raw: "secret/foo/bar/key"},
			want: VaultSecretRef{Mount: "secret", Path: "foo/bar", Key: "key"},
		},
		{
			name: "leading and trailing slashes trimmed",
			args: args{raw: "/secret/foo/key/"},
			want: VaultSecretRef{Mount: "secret", Path: "foo", Key: "key"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVaultKVv2Path(tt.args.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseVaultKVv2Path() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseVaultKVv2Path() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVaultClient_ReadVaultKVv2(t *testing.T) {
	tests := []struct {
		name        string
		ref         VaultSecretRef
		token       string
		handler     http.HandlerFunc
		want        string
		wantErr     bool
		wantErrText string
	}{
		{
			name:  "success",
			ref:   VaultSecretRef{Mount: "secret", Path: "foo", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Path; got != "/v1/secret/data/foo" {
					t.Errorf("unexpected path: %q", got)
				}
				if got := r.Header.Get("X-Vault-Token"); got != "s.token" {
					t.Errorf("bad X-Vault-Token: %q", got)
				}
				if got := r.Header.Get("Accept"); got != "application/json" {
					t.Errorf("bad Accept: %q", got)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"data": map[string]any{"password": "hunter2"},
					},
				})
			},
			want: "hunter2",
		},
		{
			name:  "mount without path",
			ref:   VaultSecretRef{Mount: "secret", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Path; got != "/v1/secret/data" {
					t.Errorf("unexpected path: %q", got)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"data": map[string]any{"password": "hunter2"},
					},
				})
			},
			want: "hunter2",
		},
		{
			name:  "not found",
			ref:   VaultSecretRef{Mount: "secret", Path: "foo", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr:     true,
			wantErrText: "vault secret secret/foo not found",
		},
		{
			name:  "not found without path omits trailing slash",
			ref:   VaultSecretRef{Mount: "secret", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			wantErr:     true,
			wantErrText: "vault secret secret not found",
		},
		{
			name:  "non 2xx status",
			ref:   VaultSecretRef{Mount: "secret", Path: "foo", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("boom"))
			},
			wantErr: true,
		},
		{
			name:  "bad json",
			ref:   VaultSecretRef{Mount: "secret", Path: "foo", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("not-json"))
			},
			wantErr: true,
		},
		{
			name:  "missing key",
			ref:   VaultSecretRef{Mount: "secret", Path: "foo", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"data": map[string]any{"other": "value"},
					},
				})
			},
			wantErr:     true,
			wantErrText: `vault secret secret/foo has no key "password"`,
		},
		{
			name:  "missing key without path omits trailing slash",
			ref:   VaultSecretRef{Mount: "secret", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"data": map[string]any{"other": "value"},
					},
				})
			},
			wantErr:     true,
			wantErrText: `vault secret secret has no key "password"`,
		},
		{
			name:  "non string value",
			ref:   VaultSecretRef{Mount: "secret", Path: "foo", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"data": map[string]any{"password": 42},
					},
				})
			},
			wantErr:     true,
			wantErrText: `vault secret secret/foo key "password" is float64, want string`,
		},
		{
			name:  "non string value without path omits trailing slash",
			ref:   VaultSecretRef{Mount: "secret", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"data": map[string]any{"password": 42},
					},
				})
			},
			wantErr:     true,
			wantErrText: `vault secret secret key "password" is float64, want string`,
		},
		{
			name:  "empty value",
			ref:   VaultSecretRef{Mount: "secret", Path: "foo", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"data": map[string]any{"password": ""},
					},
				})
			},
			wantErr:     true,
			wantErrText: `vault secret secret/foo key "password" is empty`,
		},
		{
			name:  "empty value without path omits trailing slash",
			ref:   VaultSecretRef{Mount: "secret", Key: "password"},
			token: "s.token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"data": map[string]any{"password": ""},
					},
				})
			},
			wantErr:     true,
			wantErrText: `vault secret secret key "password" is empty`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			c, err := NewVaultClient(VaultOptions{Address: srv.URL, Token: tt.token})
			if err != nil {
				t.Fatalf("NewVaultClient() error = %v", err)
			}
			got, err := c.ReadVaultKVv2(context.Background(), tt.ref)
			if (err != nil) != tt.wantErr {
				t.Fatalf("VaultClient.ReadVaultKVv2() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.wantErrText != "" && err.Error() != tt.wantErrText {
					t.Errorf("VaultClient.ReadVaultKVv2() error = %q, want %q",
						err.Error(), tt.wantErrText)
				}
				return
			}
			if got != tt.want {
				t.Errorf("VaultClient.ReadVaultKVv2() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVaultClient_ReadVaultKVv2NilContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter,
		_ *http.Request) {
	}))
	defer srv.Close()
	c, err := NewVaultClient(VaultOptions{Address: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewVaultClient() error = %v", err)
	}
	ref := VaultSecretRef{Mount: "secret", Key: "k"}
	var nilCtx context.Context //nolint:staticcheck
	if _, err := c.ReadVaultKVv2(nilCtx, ref); err == nil {
		t.Errorf("expected error for nil context, got nil")
	}
}

func TestVaultClient_ReadVaultKVv2ConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter,
		_ *http.Request) {
	}))
	c, err := NewVaultClient(VaultOptions{Address: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewVaultClient() error = %v", err)
	}
	srv.Close()
	if _, err := c.ReadVaultKVv2(context.Background(),
		VaultSecretRef{Mount: "secret", Key: "k"}); err == nil {
		t.Errorf("expected error for closed server, got nil")
	}
}

func TestVaultClient_ReadVaultKVv2BodyReadError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // DevSkim: ignore DS162092
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer ln.Close()
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte(
			"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort",
		))
	}()
	c, err := NewVaultClient(VaultOptions{Address: "http://" + ln.Addr().String(),
		Token: "tok"})
	if err != nil {
		t.Fatalf("NewVaultClient() error = %v", err)
	}
	if _, err := c.ReadVaultKVv2(context.Background(),
		VaultSecretRef{Mount: "secret", Key: "k"}); err == nil {
		t.Errorf("expected error for truncated body, got nil")
	}
}

func TestVaultClient_ReadVaultKVv2ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	c, err := NewVaultClient(VaultOptions{Address: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewVaultClient() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ReadVaultKVv2(ctx, VaultSecretRef{Mount: "secret",
		Key: "k"}); err == nil {
		t.Errorf("expected error for canceled context, got nil")
	}
}

func Test_vaultSecretPath(t *testing.T) {
	type args struct {
		ref VaultSecretRef
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "with path",
			args: args{ref: VaultSecretRef{Mount: "secret", Path: "foo/bar", Key: "k"}},
			want: "secret/foo/bar",
		},
		{
			name: "without path",
			args: args{ref: VaultSecretRef{Mount: "secret", Key: "k"}},
			want: "secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vaultSecretPath(tt.args.ref); got != tt.want {
				t.Errorf("vaultSecretPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_buildVaultKVv2Path(t *testing.T) {
	type args struct {
		ref VaultSecretRef
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "with path",
			args: args{ref: VaultSecretRef{Mount: "secret", Path: "foo/bar", Key: "k"}},
			want: "/v1/secret/data/foo/bar",
		},
		{
			name: "without path",
			args: args{ref: VaultSecretRef{Mount: "secret", Key: "k"}},
			want: "/v1/secret/data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildVaultKVv2Path(tt.args.ref); got != tt.want {
				t.Errorf("buildVaultKVv2Path() = %v, want %v", got, tt.want)
			}
		})
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return parsed
}
