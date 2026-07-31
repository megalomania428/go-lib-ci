// Package ci is documented in doc.go.
package ci

import (
	"testing"
	"time"
)

func TestEnvDefault(t *testing.T) {
	type args struct {
		key string
		def string
	}
	tests := []struct {
		name  string
		args  args
		setup func(t *testing.T)
		want  string
	}{
		{
			name: "unset falls back to default",
			args: args{key: "CI_ENV_DEFAULT_UNSET", def: "fallback"},
			want: "fallback",
		},
		{
			name: "empty falls back to default",
			args: args{key: "CI_ENV_DEFAULT_EMPTY", def: "fallback"},
			setup: func(t *testing.T) {
				t.Setenv("CI_ENV_DEFAULT_EMPTY", "")
			},
			want: "fallback",
		},
		{
			name: "set value wins",
			args: args{key: "CI_ENV_DEFAULT_SET", def: "fallback"},
			setup: func(t *testing.T) {
				t.Setenv("CI_ENV_DEFAULT_SET", "value")
			},
			want: "value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			if got := EnvDefault(tt.args.key, tt.args.def); got != tt.want {
				t.Errorf("EnvDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseIntEnv(t *testing.T) {
	type args struct {
		key string
		def int
	}
	tests := []struct {
		name    string
		args    args
		setup   func(t *testing.T)
		want    int
		wantErr bool
	}{
		{
			name: "unset falls back to default",
			args: args{key: "CI_PARSE_INT_UNSET", def: 7},
			want: 7,
		},
		{
			name: "empty falls back to default",
			args: args{key: "CI_PARSE_INT_EMPTY", def: 7},
			setup: func(t *testing.T) {
				t.Setenv("CI_PARSE_INT_EMPTY", "")
			},
			want: 7,
		},
		{
			name: "valid integer",
			args: args{key: "CI_PARSE_INT_VALID", def: 7},
			setup: func(t *testing.T) {
				t.Setenv("CI_PARSE_INT_VALID", "42")
			},
			want: 42,
		},
		{
			name: "invalid integer",
			args: args{key: "CI_PARSE_INT_INVALID", def: 7},
			setup: func(t *testing.T) {
				t.Setenv("CI_PARSE_INT_INVALID", "not-a-number")
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			got, err := ParseIntEnv(tt.args.key, tt.args.def)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseIntEnv() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("ParseIntEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseDurationEnv(t *testing.T) {
	type args struct {
		key string
		def time.Duration
	}
	tests := []struct {
		name    string
		args    args
		setup   func(t *testing.T)
		want    time.Duration
		wantErr bool
	}{
		{
			name: "unset falls back to default",
			args: args{key: "CI_PARSE_DURATION_UNSET", def: 3 * time.Second},
			want: 3 * time.Second,
		},
		{
			name: "empty falls back to default",
			args: args{key: "CI_PARSE_DURATION_EMPTY", def: 3 * time.Second},
			setup: func(t *testing.T) {
				t.Setenv("CI_PARSE_DURATION_EMPTY", "")
			},
			want: 3 * time.Second,
		},
		{
			name: "valid duration",
			args: args{key: "CI_PARSE_DURATION_VALID", def: 3 * time.Second},
			setup: func(t *testing.T) {
				t.Setenv("CI_PARSE_DURATION_VALID", "5m")
			},
			want: 5 * time.Minute,
		},
		{
			name: "invalid duration",
			args: args{key: "CI_PARSE_DURATION_INVALID", def: 3 * time.Second},
			setup: func(t *testing.T) {
				t.Setenv("CI_PARSE_DURATION_INVALID", "not-a-duration")
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}
			got, err := ParseDurationEnv(tt.args.key, tt.args.def)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDurationEnv() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("ParseDurationEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}
