package localcapacity

import (
	"testing"
	"time"
)

func TestFromEnvDisabledByDefault(t *testing.T) {
	cfg, err := FromEnv(20, "eks-linux-x64")
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}
	if cfg.Enabled {
		t.Fatal("local capacity gating must be opt-in")
	}
}

func TestFromEnvParsesSettings(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvLabels, "self-hosted, linux-x64 ,cloudCICD")
	t.Setenv(EnvPollInterval, "30s")
	t.Setenv(EnvIdleThreshold, "2")
	t.Setenv(EnvFailureThreshold, "5")
	t.Setenv(EnvExcludeNamePrefixes, "extra-pool")

	cfg, err := FromEnv(20, "eks-linux-x64")
	if err != nil {
		t.Fatalf("FromEnv() error = %v", err)
	}

	if !cfg.Enabled {
		t.Fatal("expected Enabled")
	}
	if got, want := len(cfg.Labels), 3; got != want {
		t.Fatalf("labels = %v, want %d entries", cfg.Labels, want)
	}
	if cfg.Labels[1] != "linux-x64" {
		t.Fatalf("labels not trimmed: %q", cfg.Labels[1])
	}
	if cfg.PollInterval != 30*time.Second {
		t.Fatalf("pollInterval = %s", cfg.PollInterval)
	}
	if cfg.IdleThreshold != 2 {
		t.Fatalf("idleThreshold = %d", cfg.IdleThreshold)
	}
	if cfg.FailureThreshold != 5 {
		t.Fatalf("failureThreshold = %d", cfg.FailureThreshold)
	}

	// The caller-supplied prefix must survive alongside the env-supplied one,
	// since dropping it would re-open the self-counting deadlock.
	if got, want := len(cfg.ExcludeNamePrefixes), 2; got != want {
		t.Fatalf("excludeNamePrefixes = %v, want %d entries", cfg.ExcludeNamePrefixes, want)
	}
	if cfg.ExcludeNamePrefixes[0] != "eks-linux-x64" {
		t.Fatalf("caller prefix lost: %v", cfg.ExcludeNamePrefixes)
	}
}

func TestFromEnvRejectsEmptyLabels(t *testing.T) {
	t.Setenv(EnvEnabled, "true")

	if _, err := FromEnv(20, "eks-linux-x64"); err == nil {
		t.Fatal("expected an error: an empty label set matches every runner in scope")
	}
}

func TestFromEnvRejectsTooFastPolling(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t.Setenv(EnvLabels, "linux-x64")
	t.Setenv(EnvPollInterval, "1s")

	if _, err := FromEnv(20, "eks-linux-x64"); err == nil {
		t.Fatal("expected an error for a poll interval below the floor")
	}
}

func TestFromEnvRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"enabled", EnvEnabled, "yes-please"},
		{"pollInterval", EnvPollInterval, "half a minute"},
		{"idleThreshold", EnvIdleThreshold, "two"},
		{"failureThreshold", EnvFailureThreshold, "many"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvEnabled, "true")
			t.Setenv(EnvLabels, "linux-x64")
			t.Setenv(tc.key, tc.value)

			if _, err := FromEnv(20, "eks-linux-x64"); err == nil {
				t.Fatalf("expected an error for %s=%q", tc.key, tc.value)
			}
		})
	}
}

func TestValidateRequiresPositiveMaxRunners(t *testing.T) {
	cfg := Config{
		Enabled:          true,
		Labels:           []string{"linux-x64"},
		PollInterval:     DefaultPollInterval,
		FailureThreshold: DefaultFailureThreshold,
		MaxRunners:       0,
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected an error: gating a scale set with no capacity is meaningless")
	}
}
