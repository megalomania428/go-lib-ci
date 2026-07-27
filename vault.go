// Package ci is documented in doc.go.
// cspell:ignore hashicorp cleanhttp retryablehttp mapstructure hcl kubernetes
package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VaultClient reads secrets from a KV v2 mount over the Vault HTTP API.
type VaultClient struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

// VaultOptions configures a VaultClient.
type VaultOptions struct {
	// Address is the Vault base URL, e.g. http://vault.example.com:8200.
	Address string
	// Token is the Vault token used in the X-Vault-Token header.
	Token string
	// HTTPTimeout is the per-request timeout, defaults to 30s when zero.
	// Ignored when HTTPClient is set.
	HTTPTimeout time.Duration
	// HTTPClient overrides the default *http.Client, e.g. to inject a
	// retrying transport or a test server's client. When nil, a plain
	// *http.Client with HTTPTimeout is used.
	HTTPClient *http.Client
}

// NewVaultClient validates the options and returns a ready-to-use VaultClient.
func NewVaultClient(opts VaultOptions) (*VaultClient, error) {
	if opts.Address == "" {
		return nil, errors.New("vault address is required")
	}
	if opts.Token == "" {
		return nil, errors.New("vault token is required")
	}
	parsed, err := url.Parse(opts.Address)
	if err != nil {
		return nil, fmt.Errorf("parse vault address %q: %w", opts.Address, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("vault address %q is not absolute", opts.Address)
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		timeout := opts.HTTPTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	return &VaultClient{
		baseURL:    parsed,
		token:      opts.Token,
		httpClient: httpClient,
	}, nil
}

// VaultSecretRef points at a single value inside a KV v2 secret. Path is the
// storage path without the mount and without the "/data/" prefix, Key is
// the map key inside the secret.
type VaultSecretRef struct {
	Mount string
	Path  string
	Key   string
}

// ParseVaultKVv2Path splits a slash-separated string of the form
// "<mount>/<path>/<key>" into a VaultSecretRef suitable for KV v2 lookups: the
// last segment becomes the key, everything up to the last "/" becomes the
// storage path, and the very first segment is treated as the mount name.
// A minimum of two segments (mount + key) is required.
func ParseVaultKVv2Path(raw string) (VaultSecretRef, error) {
	trimmed := strings.Trim(raw, "/")
	if trimmed == "" {
		return VaultSecretRef{}, errors.New("secret path is empty")
	}
	segments := strings.Split(trimmed, "/")
	for _, seg := range segments {
		if seg == "" {
			return VaultSecretRef{}, fmt.Errorf(
				"secret path %q contains empty segment", raw,
			)
		}
	}
	if len(segments) < 2 {
		return VaultSecretRef{}, fmt.Errorf(
			"secret path %q must contain at least <mount>/<key>", raw,
		)
	}
	ref := VaultSecretRef{
		Mount: segments[0],
		Key:   segments[len(segments)-1],
	}
	if len(segments) > 2 {
		ref.Path = strings.Join(segments[1:len(segments)-1], "/")
	}
	return ref, nil
}

// ReadVaultKVv2 reads the value stored under ref.Key inside the KV v2 secret
// at <mount>/data/<path>. It returns the raw string value and an error when
// the request fails, the secret does not exist or the key is missing.
func (c *VaultClient) ReadVaultKVv2(ctx context.Context, ref VaultSecretRef) (string,
	error) {
	rel := &url.URL{Path: buildVaultKVv2Path(ref)}
	endpoint := c.baseURL.ResolveReference(rel).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build vault request: %w", err)
	}
	req.Header.Set("X-Vault-Token", c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read vault response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("vault secret %s not found", vaultSecretPath(ref))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf(
			"vault returned %d for %s: %s",
			resp.StatusCode, endpoint, strings.TrimSpace(string(body)),
		)
	}
	var envelope struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("decode vault response: %w", err)
	}
	raw, ok := envelope.Data.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf(
			"vault secret %s has no key %q", vaultSecretPath(ref), ref.Key,
		)
	}
	value, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf(
			"vault secret %s key %q is %T, want string",
			vaultSecretPath(ref), ref.Key, raw,
		)
	}
	if value == "" {
		return "", fmt.Errorf(
			"vault secret %s key %q is empty",
			vaultSecretPath(ref), ref.Key,
		)
	}
	return value, nil
}

// vaultSecretPath renders ref as a human-readable "<mount>/<path>" string for
// error messages, omitting the trailing slash when ref.Path is empty.
func vaultSecretPath(ref VaultSecretRef) string {
	if ref.Path == "" {
		return ref.Mount
	}
	return ref.Mount + "/" + ref.Path
}

// buildVaultKVv2Path builds the request path used by the KV v2 read endpoint.
func buildVaultKVv2Path(ref VaultSecretRef) string {
	parts := []string{"v1", ref.Mount, "data"}
	if ref.Path != "" {
		parts = append(parts, ref.Path)
	}
	return "/" + strings.Join(parts, "/")
}
