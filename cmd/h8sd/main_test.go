package main

import (
	"os"
	"testing"
)

func TestGetEnv(t *testing.T) {
	t.Run("uses fallback when unset", func(t *testing.T) {
		const key = "H8SD_TEST_UNSET_PORT"
		t.Setenv(key, "temporary")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		if got := getEnv(key, "8080"); got != "8080" {
			t.Fatalf("getEnv() = %q, want %q", got, "8080")
		}
	})

	t.Run("uses environment value", func(t *testing.T) {
		const key = "H8SD_TEST_PORT"
		t.Setenv(key, "9090")
		if got := getEnv(key, "8080"); got != "9090" {
			t.Fatalf("getEnv() = %q, want %q", got, "9090")
		}
	})
}
