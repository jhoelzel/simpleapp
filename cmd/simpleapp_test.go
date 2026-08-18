package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestConfiguredPort(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "default", want: defaultHTTPPort},
		{name: "configured", value: "18080", want: 18080},
		{name: "configured with whitespace", value: " 18080 ", want: 18080},
		{name: "not numeric", value: "http", want: defaultHTTPPort},
		{name: "too low", value: "0", want: defaultHTTPPort},
		{name: "too high", value: "65536", want: defaultHTTPPort},
		{name: "reserved 5060", value: "5060", want: defaultHTTPPort},
		{name: "reserved 5061", value: "5061", want: defaultHTTPPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setCommandTestEnvironment(t, "TEST_HTTP_PORT", tt.value)
			if got := configuredPort("TEST_HTTP_PORT", defaultHTTPPort); got != tt.want {
				t.Fatalf("configuredPort() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWaitForRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if waitForRetry(ctx, time.Second) {
		t.Fatal("waitForRetry returned true for a cancelled context")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("waitForRetry did not return promptly")
	}
}

func setCommandTestEnvironment(t *testing.T, name, value string) {
	t.Helper()
	oldValue, existed := os.LookupEnv(name)
	if value == "" {
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Setenv(name, value); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, oldValue)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}
