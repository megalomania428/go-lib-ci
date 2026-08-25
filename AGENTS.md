# AGENTS.md

## Test coverage

CI (`.github/workflows/lib-test.yaml`) fails the build unless total statement coverage is exactly 100%. Any change that adds or edits code must keep coverage at 100% — new branches (including early returns, error paths and loop-chunking guards like `mpbProgressBar.AddBytes`'s overflow split) need a dedicated test case, not just a happy-path call.

Before finishing a task, verify locally with the same invocation CI uses:

```bash
go test -race -count=1 -timeout=120s -coverprofile=/tmp/cover.out \
  -covermode=atomic ./...
go tool cover -func=/tmp/cover.out | awk '/^total:/{print $3}'
```

The last line must print `100.0%`. If it does not, `go tool cover -func=/tmp/cover.out | awk '$3 != "100.0%"'` lists the exact functions missing coverage.
