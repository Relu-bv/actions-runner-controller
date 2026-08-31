package localcapacity

import (
	"context"
	"log/slog"
	"time"
)

// CapacitySetter is the subset of listener.Listener the gate drives.
//
// listener.SetMaxRunners is documented as safe to call concurrently with
// listener.Run, which is what makes this poller possible without touching the
// listener's own loop.
type CapacitySetter interface {
	SetMaxRunners(count int)
}

// Poller periodically recomputes the scale set's advertised capacity from the
// state of the local runner pool.
type Poller struct {
	lister   Lister
	setter   CapacitySetter
	config   Config
	logger   *slog.Logger
	failures int

	// lastApplied is the capacity most recently pushed to the listener, kept so
	// steady state does not log on every tick. -1 means "nothing applied yet",
	// which forces the first tick to log and apply.
	lastApplied int
}

// NewPoller wires a Lister to a CapacitySetter.
func NewPoller(lister Lister, setter CapacitySetter, config Config, logger *slog.Logger) *Poller {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Poller{
		lister:      lister,
		setter:      setter,
		config:      config,
		logger:      logger,
		lastApplied: -1,
	}
}

// Run polls until the context is cancelled. It always returns a nil error on
// cancellation: the gate is an optimisation, and its shutdown must not be the
// thing that takes the listener down.
func (p *Poller) Run(ctx context.Context) error {
	p.logger.Info("Starting local capacity poller",
		"labels", p.config.Labels,
		"pollInterval", p.config.PollInterval.String(),
		"idleThreshold", p.config.IdleThreshold,
		"maxRunners", p.config.MaxRunners,
		"excludeNamePrefixes", p.config.ExcludeNamePrefixes,
	)

	// Evaluate once before the first tick. Without this the scale set would
	// advertise full capacity for a whole interval on startup and could absorb
	// jobs the local pool was idle and waiting for.
	p.tick(ctx)

	ticker := time.NewTicker(p.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("Stopping local capacity poller")
			return nil
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

func (p *Poller) tick(ctx context.Context) {
	idle, err := p.lister.IdleRunners(ctx)
	if err != nil {
		p.handleFailure(err)
		return
	}

	p.failures = 0
	p.apply(p.desiredCapacity(idle), "idle local runners", idle)
}

// desiredCapacity is the whole policy: closed while the local pool has spare
// runners, fully open once it does not.
//
// The gate is deliberately binary rather than proportional. Advertising
// MaxRunners-minus-idle would still let the backend hand this scale set work
// while local runners sat idle, which is the exact outcome the gate exists to
// prevent. The cost is bounded: when the last idle local runner picks up a job,
// the gate opens on the next tick, so overflow is delayed by at most one poll
// interval.
func (p *Poller) desiredCapacity(idleLocal int) int {
	if idleLocal > p.config.IdleThreshold {
		return 0
	}
	return p.config.MaxRunners
}

// handleFailure implements the fail-open policy described on
// Config.FailureThreshold.
func (p *Poller) handleFailure(err error) {
	p.failures++

	if p.failures < p.config.FailureThreshold {
		p.logger.Warn("Failed to determine local runner capacity, keeping current capacity",
			"error", err,
			"consecutiveFailures", p.failures,
			"failureThreshold", p.config.FailureThreshold,
		)
		return
	}

	p.logger.Error("Failed to determine local runner capacity, failing open to full capacity",
		"error", err,
		"consecutiveFailures", p.failures,
		"maxRunners", p.config.MaxRunners,
	)
	p.apply(p.config.MaxRunners, "poller failed open", -1)
}

func (p *Poller) apply(capacity int, reason string, idleLocal int) {
	if capacity == p.lastApplied {
		return
	}

	p.setter.SetMaxRunners(capacity)
	p.lastApplied = capacity

	gate := "open"
	if capacity == 0 {
		gate = "closed"
	}

	p.logger.Info("Updated advertised scale set capacity",
		"gate", gate,
		"capacity", capacity,
		"reason", reason,
		"idleLocalRunners", idleLocal,
	)
}
