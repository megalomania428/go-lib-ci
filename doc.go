// Package ci provides reusable helpers for CI/CD Go programs: apt package
// installation with an inter-process lock and downloading GitHub release
// assets with retries, sha256 verification, progress bar and a curl-like
// low-speed watchdog. Both public and private repositories are supported.
//
// The progress bar used internally by FetchURL is also available as a
// public NewProgressBar constructor, so callers can wrap an arbitrary
// io.Reader or io.Writer with the same curl-like rendering outside of a
// download, for example to show upload progress. NewProgress builds it from
// functional options, and WithProgressElements selects any subset of the
// progress line elements, down to none:
//
//	bar := ci.NewProgress(ctx,
//		ci.WithProgressTotal(size),
//		ci.WithProgressElements(
//			ci.ProgressElementElapsed, ci.ProgressElementPercent, ci.ProgressElementETA,
//		),
//	)
//	req.Body = bar.WrapReader(body)
//	resp, err := client.Do(req)
//	if err == nil {
//		resp.Body.Close()
//	}
//	bar.Finish(err)
//
// A canceled context never makes the constructors or the bar methods panic:
// a terminal bar that can no longer be added draws nothing, and Finish
// returns without waiting for progress that will never come.
//
// Functional-option APIs provide context-aware command execution, Git helpers,
// ordered patch application, tar/xz and LZMA2 archives, GitHub release publishing
// over net/http, and Telegram Rich Messages with embedded documents. New calls
// accept a context followed by With-prefixed options; constructors omit it.
// Missing required options wrap ErrMissingOption. Existing APIs are unchanged.
package ci

// cspell:ignore LZMA
