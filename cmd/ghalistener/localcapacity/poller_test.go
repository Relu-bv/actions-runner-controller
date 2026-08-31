package localcapacity

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeLister struct {
	idle []int
	errs []error
	call int
}

func (f *fakeLister) IdleRunners(context.Context) (int, error) {
	i := f.call
	f.call++

	if i < len(f.errs) && f.errs[i] != nil {
		return 0, f.errs[i]
	}
	if i < len(f.idle) {
		return f.idle[i], nil
	}
	return 0, nil
}

type fakeSetter struct {
	applied []int
}

func (f *fakeSetter) SetMaxRunners(count int) {
	f.applied = append(f.applied, count)
}

func testConfig() Config {
	return Config{
		Enabled:          true,
		Labels:           []string{"linux-x64"},
		PollInterval:     time.Second,
		IdleThreshold:    0,
		FailureThreshold: 3,
		MaxRunners:       20,
	}
}

func TestDesiredCapacityGatesOnIdleLocalRunners(t *testing.T) {
	for _, tc := range []struct {
		name          string
		idleThreshold int
		idleLocal     int
		want          int
	}{
		{"closed while a local runner is idle", 0, 1, 0},
		{"closed while several are idle", 0, 7, 0},
		{"open once the local pool is saturated", 0, 0, 20},

		// With a reserve, the gate opens while up to IdleThreshold local
		// runners are still idle — that is the point: those are held back for
		// latency-sensitive work while the cloud absorbs the backlog.
		{"reserve holds runners free at the threshold", 2, 2, 20},
		{"reserve holds runners free below the threshold", 2, 1, 20},
		{"closed while idle exceeds the reserve", 2, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.IdleThreshold = tc.idleThreshold

			p := NewPoller(&fakeLister{}, &fakeSetter{}, cfg, nil)
			if got := p.desiredCapacity(tc.idleLocal); got != tc.want {
				t.Fatalf("desiredCapacity(%d) = %d, want %d", tc.idleLocal, got, tc.want)
			}
		})
	}
}

func TestTickAppliesCapacityAndSuppressesRepeats(t *testing.T) {
	// Two idle local runners, then none, then none again. The gate should close,
	// open, and then stay quiet rather than re-applying the same value.
	lister := &fakeLister{idle: []int{2, 0, 0}}
	setter := &fakeSetter{}
	p := NewPoller(lister, setter, testConfig(), nil)

	ctx := context.Background()
	p.tick(ctx)
	p.tick(ctx)
	p.tick(ctx)

	want := []int{0, 20}
	if len(setter.applied) != len(want) {
		t.Fatalf("applied = %v, want %v", setter.applied, want)
	}
	for i := range want {
		if setter.applied[i] != want[i] {
			t.Fatalf("applied = %v, want %v", setter.applied, want)
		}
	}
}

func TestPollerFailsOpenAfterThreshold(t *testing.T) {
	// Close the gate, then fail repeatedly. A stalled poller must not hold CI
	// closed forever, so capacity is restored once the threshold is crossed.
	boom := errors.New("github unavailable")
	lister := &fakeLister{
		idle: []int{3},
		errs: []error{nil, boom, boom, boom},
	}
	setter := &fakeSetter{}
	p := NewPoller(lister, setter, testConfig(), nil)

	ctx := context.Background()
	for i := 0; i < 4; i++ {
		p.tick(ctx)
	}

	if len(setter.applied) != 2 {
		t.Fatalf("applied = %v, want a close then a fail-open", setter.applied)
	}
	if setter.applied[0] != 0 {
		t.Fatalf("expected gate to close first, applied = %v", setter.applied)
	}
	if setter.applied[1] != 20 {
		t.Fatalf("expected fail-open to restore maxRunners, applied = %v", setter.applied)
	}
}

func TestPollerHoldsCapacityBelowFailureThreshold(t *testing.T) {
	// Transient errors below the threshold must leave the gate where it was
	// rather than flapping it open on a single blip.
	boom := errors.New("transient")
	lister := &fakeLister{
		idle: []int{3},
		errs: []error{nil, boom, boom},
	}
	setter := &fakeSetter{}
	p := NewPoller(lister, setter, testConfig(), nil)

	ctx := context.Background()
	p.tick(ctx)
	p.tick(ctx)
	p.tick(ctx)

	if len(setter.applied) != 1 || setter.applied[0] != 0 {
		t.Fatalf("applied = %v, want the gate to stay closed", setter.applied)
	}
}

func TestPollerRecoversAfterTransientFailure(t *testing.T) {
	// A success must reset the failure counter, otherwise intermittent errors
	// would eventually trip fail-open even though polling is working.
	boom := errors.New("transient")
	lister := &fakeLister{
		idle: []int{3, 0, 0, 3},
		errs: []error{nil, boom, nil, boom, nil},
	}
	setter := &fakeSetter{}
	p := NewPoller(lister, setter, testConfig(), nil)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		p.tick(ctx)
	}

	if p.failures != 0 {
		t.Fatalf("failures = %d, want 0 after a successful poll", p.failures)
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	cfg := testConfig()
	cfg.PollInterval = 10 * time.Millisecond

	p := NewPoller(&fakeLister{idle: []int{0}}, &fakeSetter{}, cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
