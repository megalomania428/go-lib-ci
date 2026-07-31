// Package ci is documented in doc.go.
package ci

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// EnvDefault returns the value of the environment variable key, or def when
// it is unset or empty. Every CI program driven by environment variables
// repeats this fallback around os.LookupEnv.
func EnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// ParseIntEnv parses the environment variable key as a base-10 int,
// returning def when it is unset or empty.
func ParseIntEnv(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer %q: %w", key, raw, err)
	}
	return v, nil
}

// ParseDurationEnv parses the environment variable key with
// time.ParseDuration, returning def when it is unset or empty.
func ParseDurationEnv(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration %q: %w", key, raw, err)
	}
	return v, nil
}
