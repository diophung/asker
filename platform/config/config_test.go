package config

import (
	"strings"
	"testing"
)

type testConfig struct {
	Addr    string `env:"ADDR" envDefault:":8080"`
	Workers int    `env:"WORKERS" envDefault:"4"`
	Debug   bool   `env:"DEBUG" envDefault:"false"`
}

func TestLoadDefaults(t *testing.T) {
	var cfg testConfig
	if err := Load("ASKER_CFGTEST_", &cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, ":8080")
	}
	if cfg.Workers != 4 {
		t.Errorf("Workers = %d, want 4", cfg.Workers)
	}
	if cfg.Debug {
		t.Errorf("Debug = true, want false")
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("ASKER_CFGTEST_ADDR", ":9999")
	t.Setenv("ASKER_CFGTEST_WORKERS", "16")
	t.Setenv("ASKER_CFGTEST_DEBUG", "true")

	var cfg testConfig
	if err := Load("ASKER_CFGTEST_", &cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, ":9999")
	}
	if cfg.Workers != 16 {
		t.Errorf("Workers = %d, want 16", cfg.Workers)
	}
	if !cfg.Debug {
		t.Errorf("Debug = false, want true")
	}
}

func TestLoadPrefixHandling(t *testing.T) {
	// An unprefixed variable must be ignored when a prefix is in effect.
	t.Setenv("ADDR", ":1111")
	t.Setenv("ASKER_CFGTEST_ADDR", ":2222")

	var cfg testConfig
	if err := Load("ASKER_CFGTEST_", &cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":2222" {
		t.Errorf("Addr = %q, want %q (prefixed var must win)", cfg.Addr, ":2222")
	}

	// With an empty prefix the bare variable applies.
	var bare testConfig
	if err := Load("", &bare); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if bare.Addr != ":1111" {
		t.Errorf("Addr = %q, want %q (empty prefix)", bare.Addr, ":1111")
	}
}

func TestLoadRequiredFieldError(t *testing.T) {
	type requiredConfig struct {
		Secret string `env:"ASKER_CFGTEST_SECRET,required"`
	}
	var cfg requiredConfig
	err := Load("", &cfg)
	if err == nil {
		t.Fatal("Load: want error for missing required variable, got nil")
	}
	if !strings.Contains(err.Error(), "ASKER_CFGTEST_SECRET") {
		t.Errorf("error %q does not mention the missing variable", err)
	}

	t.Setenv("ASKER_CFGTEST_SECRET", "s3cret")
	if err := Load("", &cfg); err != nil {
		t.Fatalf("Load with required var set: %v", err)
	}
	if cfg.Secret != "s3cret" {
		t.Errorf("Secret = %q, want %q", cfg.Secret, "s3cret")
	}
}

func TestLoadBadDst(t *testing.T) {
	cases := []struct {
		name string
		dst  any
	}{
		{"nil", nil},
		{"non-pointer", testConfig{}},
		{"pointer to non-struct", new(int)},
		{"typed nil pointer", (*testConfig)(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Load("X_", tc.dst); err == nil {
				t.Errorf("Load(%v): want error, got nil", tc.dst)
			}
		})
	}
}

func TestLoadTypeConversionError(t *testing.T) {
	t.Setenv("ASKER_CFGTEST_WORKERS", "not-a-number")
	var cfg testConfig
	if err := Load("ASKER_CFGTEST_", &cfg); err == nil {
		t.Fatal("Load: want type conversion error, got nil")
	}
}
