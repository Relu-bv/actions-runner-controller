package localcapacity

import (
	"testing"

	"github.com/google/go-github/v52/github"
)

func runner(name, status string, busy bool, labels ...string) *github.Runner {
	rl := make([]*github.RunnerLabels, 0, len(labels))
	for _, l := range labels {
		rl = append(rl, &github.RunnerLabels{Name: github.String(l)})
	}

	return &github.Runner{
		Name:   github.String(name),
		Status: github.String(status),
		Busy:   github.Bool(busy),
		Labels: rl,
	}
}

func listerWith(cfg Config) *githubLister {
	return &githubLister{config: cfg}
}

func TestIsIdleLocalRunner(t *testing.T) {
	cfg := Config{
		Labels:              []string{"self-hosted", "linux-x64"},
		ExcludeNamePrefixes: []string{"eks-linux-x64"},
	}
	l := listerWith(cfg)

	for _, tc := range []struct {
		name   string
		runner *github.Runner
		want   bool
	}{
		{
			name:   "online idle runner with all labels counts",
			runner: runner("rick", "online", false, "self-hosted", "Linux", "X64", "linux-x64"),
			want:   true,
		},
		{
			name:   "busy runner is not spare capacity",
			runner: runner("rick", "online", true, "self-hosted", "linux-x64"),
			want:   false,
		},
		{
			name:   "offline runner cannot take a job",
			runner: runner("rick", "offline", false, "self-hosted", "linux-x64"),
			want:   false,
		},
		{
			name:   "missing a required label",
			runner: runner("wendy", "online", false, "self-hosted", "builder"),
			want:   false,
		},
		{
			// The deadlock guard: our own ephemeral runners carry the same
			// labels, and counting them would keep the gate shut forever.
			name:   "this scale set's own runner is excluded",
			runner: runner("eks-linux-x64-abc123", "online", false, "self-hosted", "linux-x64"),
			want:   false,
		},
		{
			name:   "nil runner is ignored",
			runner: nil,
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.isIdleLocalRunner(tc.runner); got != tc.want {
				t.Fatalf("isIdleLocalRunner() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLabelMatchingIsCaseInsensitive(t *testing.T) {
	// GitHub normalises the reserved labels ("Linux", "X64") but preserves the
	// case of custom ones, so a case-sensitive match would silently miss
	// runners depending on how they were registered.
	l := listerWith(Config{Labels: []string{"Linux", "cloudCICD"}})

	r := runner("dustin", "online", false, "self-hosted", "linux", "cloudcicd")
	if !l.isIdleLocalRunner(r) {
		t.Fatal("expected case-insensitive label matching to count the runner")
	}
}

func TestHasAllLabelsRequiresEveryLabel(t *testing.T) {
	r := runner("x", "online", false, "a", "b")

	if !hasAllLabels(r, []string{"a", "b"}) {
		t.Fatal("expected exact label set to match")
	}
	if !hasAllLabels(r, []string{"a"}) {
		t.Fatal("expected subset requirement to match")
	}
	if hasAllLabels(r, []string{"a", "b", "c"}) {
		t.Fatal("expected superset requirement not to match")
	}
}
