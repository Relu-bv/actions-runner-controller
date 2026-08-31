// Package localcapacity gates a runner scale set's advertised capacity on the
// availability of runners that ARC does not manage — typically static,
// long-lived runners on on-premises hardware.
//
// # WHY THIS WORKS
//
// A scale set reports how many jobs it can accept on every long poll
// (listener.Listener passes its max runners to Client.GetMessage). Since the
// capacity propagation added in actions/actions-runner-controller#3431, the
// Actions service honours that number and will not assign jobs to a scale set
// that reports zero capacity. Jobs then stay queued and are picked up by any
// other online, idle runner whose labels match — which is exactly the
// on-premises pool we want to saturate first.
//
// So the gate is not a scheduling hack: it drives the same capacity signal the
// backend already uses for flow control. While any local runner is idle we
// report zero, and the backend routes to the local pool. Once the local pool is
// saturated we report the configured maximum and absorb the overflow.
//
// This also gives drain-on-idle for free. Scale set runners are ephemeral — one
// job each, then exit — so closing the gate simply stops new runners being
// created. In-flight jobs finish normally; nothing is evicted.
package localcapacity

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables read by FromEnv. They are deliberately environment
// driven rather than part of the listener's JSON config: the gha-runner-scale-set
// chart already lets operators set env on the listener container via
// listenerTemplate, so no CRD, controller, or chart change is needed to
// configure this. That keeps the diff against upstream confined to
// cmd/ghalistener and makes rebases cheap.
const (
	EnvEnabled             = "LOCAL_CAPACITY_ENABLED"
	EnvLabels              = "LOCAL_CAPACITY_LABELS"
	EnvPollInterval        = "LOCAL_CAPACITY_POLL_INTERVAL"
	EnvIdleThreshold       = "LOCAL_CAPACITY_IDLE_THRESHOLD"
	EnvExcludeNamePrefixes = "LOCAL_CAPACITY_EXCLUDE_NAME_PREFIXES"
	EnvFailureThreshold    = "LOCAL_CAPACITY_FAILURE_THRESHOLD"
)

const (
	DefaultPollInterval     = 15 * time.Second
	DefaultFailureThreshold = 3

	// Polling faster than this is refused. Every tick is a REST call against
	// the runners API, and on enterprise scope that call can only be made with
	// a classic PAT (5,000 requests/hour), not a GitHub App (15,000/hour).
	// See Config.Validate.
	minPollInterval = 5 * time.Second
)

// Config controls the capacity gate.
type Config struct {
	// Enabled turns the gate on. When false the listener behaves exactly like
	// upstream and always advertises MaxRunners.
	Enabled bool

	// Labels a runner must carry, ALL of them, to count towards local capacity.
	// Matching is case-insensitive because GitHub normalises some labels
	// itself ("Linux", "X64") while preserving the case of custom ones.
	//
	// Required when Enabled: an empty label set would match every runner in the
	// scope, including runners belonging to unrelated pools, and would gate this
	// scale set on their idleness.
	Labels []string

	// ExcludeNamePrefixes drops runners whose name starts with any of these
	// prefixes before counting.
	//
	// This is load bearing. The runners API returns this scale set's OWN
	// ephemeral runners alongside the static ones, and they carry the same
	// labels by construction. Counting them would create a feedback loop: an
	// idle runner of ours would close the gate, which stops new work being
	// assigned, which keeps it idle. The caller seeds this with the ephemeral
	// runner set name, which every runner this scale set creates is prefixed
	// with.
	ExcludeNamePrefixes []string

	// PollInterval is how often local runner state is refreshed. It bounds the
	// worst case latency before overflow starts: when the last idle local
	// runner picks up a job, the gate opens on the next tick.
	PollInterval time.Duration

	// IdleThreshold is the number of idle local runners tolerated while the
	// gate stays open. The gate closes when idleLocal > IdleThreshold.
	//
	// Zero — the default — means "no cloud runners while any local runner is
	// idle", which is the strict reading of "only burst when the local pool is
	// full". Raise it to keep a small local reserve free for latency sensitive
	// jobs while the cloud absorbs a backlog.
	IdleThreshold int

	// FailureThreshold is how many consecutive polling errors are tolerated
	// before the gate FAILS OPEN and restores full capacity.
	//
	// Failing open is deliberate. A closed gate with a broken poller would stall
	// CI indefinitely and look exactly like a GitHub outage; running jobs in the
	// cloud that could have run locally merely costs money. Cost is recoverable,
	// a wedged pipeline during an incident is not.
	FailureThreshold int

	// MaxRunners is the scale set's configured ceiling — the capacity restored
	// when the gate is open.
	MaxRunners int
}

// FromEnv builds a Config from the environment. maxRunners and
// excludeNamePrefixes come from the listener's own configuration rather than
// the environment, so they cannot drift from the scale set they describe.
func FromEnv(maxRunners int, excludeNamePrefixes ...string) (Config, error) {
	cfg := Config{
		PollInterval:        DefaultPollInterval,
		FailureThreshold:    DefaultFailureThreshold,
		MaxRunners:          maxRunners,
		ExcludeNamePrefixes: excludeNamePrefixes,
	}

	if raw, ok := os.LookupEnv(EnvEnabled); ok {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvEnabled, err)
		}
		cfg.Enabled = enabled
	}

	if !cfg.Enabled {
		return cfg, nil
	}

	cfg.Labels = splitAndTrim(os.Getenv(EnvLabels))

	if raw, ok := os.LookupEnv(EnvPollInterval); ok {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvPollInterval, err)
		}
		cfg.PollInterval = interval
	}

	if raw, ok := os.LookupEnv(EnvIdleThreshold); ok {
		threshold, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvIdleThreshold, err)
		}
		cfg.IdleThreshold = threshold
	}

	if raw, ok := os.LookupEnv(EnvFailureThreshold); ok {
		threshold, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvFailureThreshold, err)
		}
		cfg.FailureThreshold = threshold
	}

	if extra := splitAndTrim(os.Getenv(EnvExcludeNamePrefixes)); len(extra) > 0 {
		cfg.ExcludeNamePrefixes = append(cfg.ExcludeNamePrefixes, extra...)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Validate reports configuration that would make the gate unsafe or useless.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}

	if len(c.Labels) == 0 {
		return fmt.Errorf("%s must list at least one label when %s is true: an empty set matches every runner in scope", EnvLabels, EnvEnabled)
	}

	if c.PollInterval < minPollInterval {
		return fmt.Errorf("%s must be at least %s, got %s", EnvPollInterval, minPollInterval, c.PollInterval)
	}

	if c.IdleThreshold < 0 {
		return fmt.Errorf("%s cannot be negative, got %d", EnvIdleThreshold, c.IdleThreshold)
	}

	if c.FailureThreshold < 1 {
		return fmt.Errorf("%s must be at least 1, got %d", EnvFailureThreshold, c.FailureThreshold)
	}

	if c.MaxRunners <= 0 {
		return fmt.Errorf("local capacity gating requires a positive maxRunners, got %d", c.MaxRunners)
	}

	return nil
}

func splitAndTrim(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}
