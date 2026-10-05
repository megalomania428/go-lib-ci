// Package ci is documented in doc.go.
package ci

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
	"strings"
	"testing"
	"time"
)

func TestNewTelegramClient(t *testing.T) {
	if _, err := NewTelegramClient(); !errors.Is(err, ErrMissingOption) {
		t.Fatal(err)
	}
	if _, err := NewTelegramClient(WithTelegramToken(" ")); err == nil {
		t.Fatal("empty bot token")
	}
	c, err := NewTelegramClient(
		WithTelegramToken("123:token"),
		WithTelegramHTTPClient(nil),
		WithTelegramStderr(nil),
	)
	if err != nil || c.server != "https://api.telegram.org" || c.http == nil ||
		c.stderr == nil {
		t.Fatalf("%v %v", c, err)
	}
	if tr, ok := c.http.Transport.(*http.Transport); !ok ||
		tr.ResponseHeaderTimeout != defaultConnectTimeout {
		t.Fatalf("default HTTP client has no timeout: %+v", c.http.Transport)
	}
	if got := (&TelegramAPIError{
		Code:        400,
		Description: "bad",
	}).Error(); got != "Telegram API 400: bad" {
		t.Fatal(got)
	}
}

func telegramTestClient(
	t *testing.T,
	server *httptest.Server,
	backoff BackoffOptions,
) *TelegramClient {
	t.Helper()
	c, err := NewTelegramClient(
		WithTelegramToken("123:token"),
		WithTelegramServerURL(server.URL+"/"),
		WithTelegramHTTPClient(
			server.Client(),
		),
		WithTelegramBackoff(backoff),
		WithTelegramStderr(io.Discard),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestTelegramClient_SendRichPost(t *testing.T) {
	ctx := context.Background()
	files := []string{
		filepath.Join(t.TempDir(), "a[1].txz"),
		filepath.Join(t.TempDir(), "a[1].txz"),
	}
	for i, file := range files {
		if err := os.WriteFile(file, []byte(fmt.Sprint(i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.URL.Path != "/bot123:token/sendRichMessage" {
				t.Errorf("unexpected call %s", r.URL.Path)
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = r.MultipartForm.RemoveAll() }()
			if r.FormValue("chat_id") != "@channel" ||
				r.FormValue("disable_notification") != "true" {
				t.Error("post fields")
			}
			var rich struct {
				Markdown string `json:"markdown"`
				Media    []struct {
					ID    string `json:"id"`
					Media struct {
						Type  string `json:"type"`
						Media string `json:"media"`
					} `json:"media"`
				} `json:"media"`
			}
			if err := json.Unmarshal([]byte(r.FormValue("rich_message")), &rich); err != nil {
				t.Error(err)
			}
			if len(rich.Media) != 2 ||
				rich.Markdown != "# Notes\n\n[a\\[1\\]\\.txz](tg://document?id=doc"+
					"0)\n\n[a\\[1\\]\\.txz](tg://document?id=doc1)" {
				t.Errorf("rich %+v", rich)
			}
			for i := range files {
				id := fmt.Sprintf("doc%d", i)
				name := id + "-a[1].txz"
				if rich.Media[i].ID != id || rich.Media[i].Media.Media != "attach://"+name ||
					rich.Media[i].Media.Type != "document" {
					t.Error("media reference")
				}
				file, _, err := r.FormFile(name)
				if err != nil {
					t.Error(err)
					continue
				}
				data, err := io.ReadAll(file)
				_ = file.Close()
				if err != nil || string(data) != fmt.Sprint(i) {
					t.Errorf("file %q %v", data, err)
				}
			}
			if calls == 1 {
				w.WriteHeader(429)
				_, _ = io.WriteString(w, `{"ok":false,"error_code":429}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		}),
	)
	defer server.Close()
	c := telegramTestClient(t, server, fastBackoff())
	if calls != 0 {
		t.Fatal("constructor called getMe")
	}
	for _, opts := range [][]PostOption{nil, {WithPostChat("@channel")}} {
		if err := c.SendRichPost(ctx, opts...); !errors.Is(err, ErrMissingOption) {
			t.Fatal(err)
		}
	}
	err := c.SendRichPost(
		ctx,
		WithPostChat("@channel"),
		WithPostMarkdown("# Notes"),
		WithPostDocuments(files...),
		WithPostSilent(true),
	)
	if err != nil || calls != 2 {
		t.Fatalf("%d %v", calls, err)
	}
	if err := c.SendRichPost(
		ctx,
		WithPostChat("x"),
		WithPostMarkdown("notes"),
		WithPostDocuments(files[0], "/missing/document"),
	); err == nil {
		t.Fatal("open error")
	}
	if err := c.SendRichPost(
		ctx,
		WithPostChat("x"),
		WithPostMarkdown("notes"),
		WithPostDocuments(t.TempDir()),
	); err == nil {
		t.Fatal("read error")
	}
	if got := escapeRichLabel(`a\b[c]_d*é$.txt`); got != `a\\b\[c\]\_d\*é\$\.txt` {
		t.Fatal(got)
	}
}
func TestTelegramClient_APIError(t *testing.T) {
	for _, tt := range []struct {
		status, code int
		body         string
		calls        int
	}{
		{400, 400, `{"ok":false,"error_code":400,"description":"bad Markdown"}`, 1},
		{401, 401, `{"ok":false,"error_code":401}`, 1},
		{402, 402, `{"ok":false,"error_code":402}`, 1},
		{403, 403, `{"ok":false,"error_code":403}`, 1},
		{404, 404, `{"ok":false,"error_code":404}`, 1},
		{409, 409, `{"ok":false,"error_code":409}`, 1},
		{408, 408, `{"ok":false,"error_code":408}`, 1},
		{200, 400, `{"ok":false,"error_code":400}`, 1},
		{400, 400, `invalid JSON`, 1},
		{500, 500, `invalid JSON`, 1},
		{503, 503, `{"ok":false,"error_code":400}`, 1},
		{200, 500, `{}`, 1},
		{429, 429, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`, 2},
	} {
		t.Run(fmt.Sprintf("%d/%d/%s", tt.status, tt.code, tt.body), func(t *testing.T) {
			calls := 0
			var first time.Time
			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls++
					if calls == 1 {
						first = time.Now()
						w.WriteHeader(tt.status)
						_, _ = io.WriteString(w, tt.body)
						return
					}
					if tt.status == 429 && time.Since(first) < time.Second {
						t.Error("ignored retry_after")
					}
					_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
				}),
			)
			defer server.Close()
			backoff := fastBackoff()
			if tt.status == 429 {
				backoff.MaxRetriesTime = time.Minute
			}
			c := telegramTestClient(t, server, backoff)
			err := c.SendRichPost(
				context.Background(),
				WithPostChat("-1001"),
				WithPostMarkdown("notes"),
			)
			if calls != tt.calls || (err != nil) != (tt.calls == 1) {
				t.Fatalf("calls %d err %v", calls, err)
			}
			if err != nil {
				var api *TelegramAPIError
				if !errors.As(err, &api) || api.Code != tt.code {
					t.Fatalf("%v", err)
				}
			}
		})
	}
}
func TestTelegramClient_TransportErrors(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		body      io.Reader
		network   string
		permanent bool
		calls     int
	}{
		{"dial", 0, nil, "dial", false, 2},
		{"dial terminal", 0, nil, "dial", true, 2},
		{"network", 0, nil, "read", true, 1},
		{"read", 200, errorReader{}, "", true, 1},
		{"json", 200, strings.NewReader("invalid JSON"), "", true, 1},
		{"result", 200, strings.NewReader(`{"ok":true,"result":42}`), "", true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			var diagnostic bytes.Buffer
			h := &http.Client{
				Transport: testRoundTrip(func(req *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 || tt.permanent {
						if tt.network != "" {
							return nil, &net.OpError{Op: tt.network, Net: "tcp",
								Err: fmt.Errorf("connection failed: %s", req.URL)}
						}
						return &http.Response{StatusCode: tt.status, Body: io.NopCloser(tt.body)}, nil
					}
					return &http.Response{
						StatusCode: 200,
						Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
					}, nil
				}),
			}
			c, err := NewTelegramClient(
				WithTelegramToken("123:secret"),
				WithTelegramHTTPClient(h),
				WithTelegramBackoff(fastBackoff()),
				WithTelegramStderr(&diagnostic),
			)
			if err != nil {
				t.Fatal(err)
			}
			err = c.SendRichPost(context.Background(), WithPostChat("x"),
				WithPostMarkdown("notes"))
			if (err != nil) != tt.permanent || (calls < 2) != (tt.calls == 1) {
				t.Fatalf("%d %v", calls, err)
			}
			if strings.Contains(diagnostic.String(), "123:secret") ||
				(err != nil && strings.Contains(err.Error(), "123:secret")) {
				t.Fatalf("bot token leaked: %s %v", &diagnostic, err)
			}
			if tt.network == "dial" &&
				!strings.Contains(diagnostic.String(), "[REDACTED]") {
				t.Fatalf("missing redacted transport diagnostics: %s", &diagnostic)
			}
		})
	}
	c, _ := NewTelegramClient(
		WithTelegramToken("token"),
		WithTelegramServerURL("http://["),
		WithTelegramBackoff(BackoffOptions{MaxRetriesTime: -1}),
		WithTelegramStderr(io.Discard),
	)
	if err := c.SendRichPost(
		context.Background(),
		WithPostChat("x"),
		WithPostMarkdown("notes"),
	); err == nil {
		t.Fatal("request error")
	}
}
func TestTelegramRetryAfterDoesNotLeak(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {
		calls++
		switch calls {
		case 1:
			w.WriteHeader(429)
			_, _ = io.WriteString(w,
				`{"ok":false,"error_code":429,"parameters":{"retry_after":10}}`)
		case 2:
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":429}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
	defer server.Close()
	var pauses []time.Duration
	original := telegramPause
	telegramPause = func(_ context.Context, delay time.Duration) error {
		pauses = append(pauses, delay)
		return nil
	}
	t.Cleanup(func() { telegramPause = original })
	backoff := fastBackoff()
	backoff.MaxRetriesTime = time.Minute
	c := telegramTestClient(t, server, backoff)
	err := c.SendRichPost(context.Background(), WithPostChat("x"),
		WithPostMarkdown("notes"))
	if err != nil || calls != 3 || len(pauses) != 1 || pauses[0] != 10*time.Second {
		t.Fatalf("retry_after leaked to unrelated errors: %v %d %v", err, calls, pauses)
	}
}
func TestTelegramRetryAfterOverBudget(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {
		calls++
		w.WriteHeader(429)
		_, _ = io.WriteString(w,
			`{"ok":false,"error_code":429,"parameters":{"retry_after":3600}}`)
	}))
	defer server.Close()
	original := telegramPause
	telegramPause = func(context.Context, time.Duration) error {
		t.Fatal("pause exceeds the retry budget")
		return nil
	}
	t.Cleanup(func() { telegramPause = original })
	c := telegramTestClient(t, server, fastBackoff())
	err := c.SendRichPost(context.Background(), WithPostChat("x"),
		WithPostMarkdown("notes"))
	var api *TelegramAPIError
	if !errors.As(err, &api) || api.Code != 429 || calls != 1 {
		t.Fatalf("over-budget retry_after: %v %d", err, calls)
	}
}
func Test_waitTelegram(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitTelegram(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := waitTelegram(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(429)
			_, _ = io.WriteString(
				w,
				`{"ok":false,"error_code":429,"parameters":{"retry_after":10}}`,
			)
		}),
	)
	defer server.Close()
	backoff := fastBackoff()
	backoff.MaxRetriesTime = time.Minute
	c := telegramTestClient(t, server, backoff)
	deadline, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := c.SendRichPost(
		deadline,
		WithPostChat("x"),
		WithPostMarkdown("notes"),
	); !errors.Is(
		err,
		context.DeadlineExceeded,
	) {
		t.Fatalf("%v", err)
	}
}

func Test_telegramError_Error(t *testing.T) {
	type fields struct {
		cause error
		token string
	}
	tests := []struct {
		name   string
		fields fields
		want   string
	}{
		{
			name: "redacts all token occurrences",
			fields: fields{cause: errors.New("POST /bot123:secret: 123:secret failed"),
				token: "123:secret"},
			want: "POST /bot[REDACTED]: [REDACTED] failed",
		},
		{
			name:   "keeps diagnostics without token",
			fields: fields{cause: context.DeadlineExceeded, token: "123:secret"},
			want:   context.DeadlineExceeded.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &telegramError{
				cause: tt.fields.cause,
				token: tt.fields.token,
			}
			if got := e.Error(); got != tt.want {
				t.Errorf("telegramError.Error() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_telegramError_Unwrap(t *testing.T) {
	type fields struct {
		cause error
		token string
	}
	tests := []struct {
		name    string
		fields  fields
		wantErr bool
	}{
		{name: "preserves cancellation", fields: fields{cause: context.Canceled},
			wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &telegramError{
				cause: tt.fields.cause,
				token: tt.fields.token,
			}
			if err := e.Unwrap(); (err != nil) != tt.wantErr {
				t.Errorf("telegramError.Unwrap() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !errors.Is(e, tt.fields.cause) {
				t.Errorf("original error lost: %v", e)
			}
		})
	}
}
func Test_telegramHTTP_Do(t *testing.T) {
	original := responseStallTimeout
	responseStallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { responseStallTimeout = original })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		if r.URL.Path != "/stall" {
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer server.Close()
	type fields struct {
		client *http.Client
	}
	type args struct {
		path string
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    string
		wantErr string
	}{
		{name: "complete body", fields: fields{server.Client()}, args: args{"/ok"},
			want: `{"ok":true,"result":{}}`},
		{name: "stalled body", fields: fields{server.Client()}, args: args{"/stall"},
			wantErr: "read Telegram response: response stalled for 50ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &telegramHTTP{
				client: tt.fields.client,
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				server.URL+tt.args.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := h.Do(req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("telegramHTTP.Do() error = %v, wantErr %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("telegramHTTP.Do() error = %v", err)
			}
			data, err := io.ReadAll(got.Body)
			if err != nil || string(data) != tt.want {
				t.Errorf("telegramHTTP.Do() body = %q, error %v, want %q", data, err, tt.want)
			}
		})
	}
}
