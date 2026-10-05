// Package ci is documented in doc.go.
package ci

// cspell:ignore apihttp

import (
	"bytes"
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

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

var telegramPause = waitTelegram

// TelegramAPIError represents an unsuccessful Telegram API response.
type TelegramAPIError struct {
	// Code is Telegram's error_code or the unsuccessful HTTP status.
	Code int
	// Description is the diagnostic supplied by the Bot API.
	Description string
	retryAfter  time.Duration
}

// Error returns the API code and description.
func (e *TelegramAPIError) Error() string {
	return fmt.Sprintf("Telegram API %d: %s", e.Code, e.Description)
}

// TelegramClient sends rich posts without polling or an initialization request.
type TelegramClient struct {
	token, server string
	http          *http.Client
	backoff       BackoffOptions
	stderr        io.Writer
	bot           *bot.Bot
}

// TelegramOption configures NewTelegramClient.
type TelegramOption func(*TelegramClient)

// WithTelegramToken sets the required bot token.
func WithTelegramToken(token string) TelegramOption {
	return func(c *TelegramClient) { c.token = token }
}

// WithTelegramServerURL overrides the Bot API server.
func WithTelegramServerURL(u string) TelegramOption {
	return func(c *TelegramClient) { c.server = strings.TrimRight(u, "/") }
}

// WithTelegramHTTPClient overrides the HTTP client.
func WithTelegramHTTPClient(h *http.Client) TelegramOption {
	return func(c *TelegramClient) { c.http = h }
}

// WithTelegramBackoff overrides the retry schedule.
func WithTelegramBackoff(b BackoffOptions) TelegramOption {
	return func(c *TelegramClient) { c.backoff = b }
}

// WithTelegramStderr sets diagnostic output.
func WithTelegramStderr(w io.Writer) TelegramOption {
	return func(c *TelegramClient) { c.stderr = w }
}

// NewTelegramClient validates configuration and skips getMe.
func NewTelegramClient(opts ...TelegramOption) (*TelegramClient, error) {
	c := &TelegramClient{server: "https://api.telegram.org",
		http: newAPIHTTPClient(), stderr: os.Stderr}
	for _, opt := range opts {
		opt(c)
	}
	if c.token == "" {
		return nil, fmt.Errorf("NewTelegramClient: token: %w", ErrMissingOption)
	}
	if c.http == nil {
		c.http = newAPIHTTPClient()
	}
	if c.stderr == nil {
		c.stderr = os.Stderr
	}
	b, err := bot.New(c.token, bot.WithSkipGetMe(), bot.WithServerURL(c.server),
		bot.WithHTTPClient(0, &telegramHTTP{c.http}))
	if err != nil {
		return nil, fmt.Errorf("initialize Telegram bot: %w", err)
	}
	c.bot = b
	return c, nil
}

type postConfig struct {
	chat, markdown string
	documents      []string
	silent         bool
}

// PostOption configures SendRichPost.
type PostOption func(*postConfig)

// WithPostChat sets the required @username or numeric chat ID.
func WithPostChat(chat string) PostOption {
	return func(c *postConfig) { c.chat = chat }
}

// WithPostMarkdown sets the required Rich Markdown text.
func WithPostMarkdown(md string) PostOption {
	return func(c *postConfig) { c.markdown = md }
}

// WithPostDocuments sets ordered files embedded at the end of the post.
func WithPostDocuments(paths ...string) PostOption {
	return func(c *postConfig) { c.documents = paths }
}

// WithPostSilent disables notification sound.
func WithPostSilent(on bool) PostOption { return func(c *postConfig) { c.silent = on } }

// SendRichPost sends one Rich Message, reopening every document for each retry.
func (c *TelegramClient) SendRichPost(ctx context.Context, opts ...PostOption) error {
	p := &postConfig{}
	for _, opt := range opts {
		opt(p)
	}
	if p.chat == "" {
		return fmt.Errorf("SendRichPost: chat: %w", ErrMissingOption)
	}
	if strings.TrimSpace(p.markdown) == "" {
		return fmt.Errorf("SendRichPost: markdown: %w", ErrMissingOption)
	}
	return retryAPI(ctx, RetryOptions{Name: "Telegram rich post", BackoffOptions: c.backoff,
		Stderr: c.stderr, IdleCloser: transportIdleCloser(c.http)}, telegramPause,
		func(ctx context.Context) error {
			err := c.sendPost(ctx, p)
			var api *TelegramAPIError
			if errors.As(err, &api) && api.Code == 429 {
				return &retryPause{err, api.retryAfter}
			}
			// A lost response may hide a delivered post, so only unsent posts are retried.
			if err != nil && !requestUnsent(err) {
				return &stopRetry{err}
			}
			return err
		})
}
func waitTelegram(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (c *TelegramClient) sendPost(ctx context.Context, p *postConfig) error {
	rich := models.InputRichMessage{Markdown: p.markdown}
	for i, path := range p.documents {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open Telegram document: %w", err)
		}
		defer func() { _ = file.Close() }()
		id := fmt.Sprintf("doc%d", i)
		// Unique part names avoid collisions even when base names repeat.
		part := id + "-" + filepath.Base(path)
		document := &models.InputMediaDocument{
			Media: "attach://" + part, MediaAttachment: file,
		}
		rich.Media = append(rich.Media, models.InputRichMessageMedia{ID: id, Media: document})
		label := escapeRichLabel(filepath.Base(path))
		rich.Markdown += "\n\n[" + label + "](tg://document?id=" + id + ")"
	}
	_, err := c.bot.SendRichMessage(ctx, &bot.SendRichMessageParams{ChatID: p.chat,
		RichMessage: rich, DisableNotification: p.silent})
	if err != nil {
		return &telegramError{cause: err, token: c.token}
	}
	return nil
}

// telegramError redacts the bot token while preserving the original error chain.
type telegramError struct {
	cause error
	token string
}

func (e *telegramError) Error() string {
	return strings.ReplaceAll(e.cause.Error(), e.token, "[REDACTED]")
}
func (e *telegramError) Unwrap() error { return e.cause }

// escapeRichLabel escapes every ASCII punctuation character, which GFM-compatible Rich
// Markdown allows, so names like a_b*c$.txt are never parsed as markup.
func escapeRichLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// telegramHTTP preserves arbitrary API error codes that bot's sentinel errors omit.
type telegramHTTP struct{ client *http.Client }

func (h *telegramHTTP) Do(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	resp, err := h.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	stall := newStallBody(resp.Body, cancel)
	defer func() { _ = stall.Close() }()
	body, err := io.ReadAll(stall)
	if err != nil {
		return nil, fmt.Errorf("read Telegram response: %w", err)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Code        int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	decodeErr := json.Unmarshal(body, &result)
	if resp.StatusCode >= 400 || (decodeErr == nil && !result.OK) {
		code := result.Code
		if code == 0 || resp.StatusCode >= 500 {
			code = resp.StatusCode
		}
		if code < 400 {
			code = 500
		}
		description := result.Description
		if description == "" {
			description = strings.TrimSpace(string(body))
		}
		return nil, &TelegramAPIError{Code: code, Description: description,
			retryAfter: time.Duration(result.Parameters.RetryAfter) * time.Second}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}
