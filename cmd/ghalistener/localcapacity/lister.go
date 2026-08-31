package localcapacity

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/actions/actions-runner-controller/apis/actions.github.com/v1alpha1/appconfig"
	"github.com/actions/actions-runner-controller/github/actions"
	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v52/github"
)

// runnersPerPage is the maximum the runners API accepts. The listing is a
// single hot path executed on every tick, so it is paged as coarsely as
// possible to keep REST budget use down.
const runnersPerPage = 100

// Lister reports how many runners outside this scale set are online, idle, and
// carry the required labels.
type Lister interface {
	IdleRunners(ctx context.Context) (int, error)
}

// githubLister implements Lister against the self-hosted runners REST API.
type githubLister struct {
	client   *github.Client
	ghConfig *actions.GitHubConfig
	config   Config
}

// NewLister builds a Lister for the scope described by ghConfig.
//
// Note the auth asymmetry, which is a GitHub constraint rather than a choice
// here: the enterprise runners endpoints reject GitHub App credentials
// entirely and require a classic PAT with manage_runners:enterprise, while
// organization and repository scopes accept either.
func NewLister(ghConfig *actions.GitHubConfig, appCfg *appconfig.AppConfig, config Config) (Lister, error) {
	client, err := newRESTClient(appCfg, ghConfig)
	if err != nil {
		return nil, err
	}

	return &githubLister{
		client:   client,
		ghConfig: ghConfig,
		config:   config,
	}, nil
}

func newRESTClient(appCfg *appconfig.AppConfig, ghConfig *actions.GitHubConfig) (*github.Client, error) {
	if appCfg == nil {
		return nil, fmt.Errorf("no GitHub credentials available for the local capacity poller")
	}

	var httpClient *http.Client

	switch {
	case appCfg.Token != "":
		httpClient = nil // filled in by NewTokenClient below

	case appCfg.AppPrivateKey != "":
		if ghConfig.Scope == actions.GitHubScopeEnterprise {
			return nil, fmt.Errorf(
				"local capacity gating at enterprise scope requires a personal access token with manage_runners:enterprise; " +
					"the enterprise runners API does not accept GitHub App credentials",
			)
		}

		// ghinstallation only accepts a numeric app ID. AppConfig.AppID may also
		// hold a GitHub App *client ID* (the "Iv23li..." form), which the scale
		// set client supports but this REST path cannot.
		appID, err := strconv.ParseInt(appCfg.AppID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf(
				"local capacity gating needs a numeric GitHub App id (got %q); use the app id rather than the client id, or authenticate with a personal access token",
				appCfg.AppID,
			)
		}

		transport, err := ghinstallation.New(
			http.DefaultTransport,
			appID,
			appCfg.AppInstallationID,
			[]byte(appCfg.AppPrivateKey),
		)
		if err != nil {
			return nil, fmt.Errorf("creating GitHub App transport: %w", err)
		}

		httpClient = &http.Client{Transport: transport}

	default:
		return nil, fmt.Errorf("no usable GitHub credentials for the local capacity poller")
	}

	if httpClient == nil {
		return github.NewTokenClient(context.Background(), appCfg.Token), nil
	}

	return github.NewClient(httpClient), nil
}

// IdleRunners counts online, non-busy runners in scope that carry every
// configured label and are not part of this scale set.
func (l *githubLister) IdleRunners(ctx context.Context) (int, error) {
	opts := &github.ListOptions{PerPage: runnersPerPage}

	var idle int
	for {
		runners, resp, err := l.listPage(ctx, opts)
		if err != nil {
			return 0, err
		}

		for _, runner := range runners.Runners {
			if l.isIdleLocalRunner(runner) {
				idle++
			}
		}

		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return idle, nil
}

func (l *githubLister) listPage(ctx context.Context, opts *github.ListOptions) (*github.Runners, *github.Response, error) {
	switch l.ghConfig.Scope {
	case actions.GitHubScopeEnterprise:
		return l.client.Enterprise.ListRunners(ctx, l.ghConfig.Enterprise, opts)
	case actions.GitHubScopeOrganization:
		return l.client.Actions.ListOrganizationRunners(ctx, l.ghConfig.Organization, opts)
	case actions.GitHubScopeRepository:
		return l.client.Actions.ListRunners(ctx, l.ghConfig.Organization, l.ghConfig.Repository, opts)
	default:
		return nil, nil, fmt.Errorf("unsupported GitHub scope %v", l.ghConfig.Scope)
	}
}

// isIdleLocalRunner decides whether a runner counts as spare local capacity.
//
// Every condition here is a reason NOT to burst to the cloud on this runner's
// behalf, so all of them must hold.
func (l *githubLister) isIdleLocalRunner(runner *github.Runner) bool {
	if runner == nil {
		return false
	}

	// An offline runner cannot take a job, so it is not capacity. Note that
	// scale set runners which have finished and unregistered can linger here
	// briefly in the "offline" state; excluding them is correct anyway.
	if !strings.EqualFold(runner.GetStatus(), "online") {
		return false
	}

	if runner.GetBusy() {
		return false
	}

	// Drop this scale set's own ephemeral runners — see Config.ExcludeNamePrefixes
	// for why counting them would deadlock the gate.
	name := runner.GetName()
	for _, prefix := range l.config.ExcludeNamePrefixes {
		if prefix != "" && strings.HasPrefix(name, prefix) {
			return false
		}
	}

	return hasAllLabels(runner, l.config.Labels)
}

func hasAllLabels(runner *github.Runner, required []string) bool {
	present := make(map[string]struct{}, len(runner.Labels))
	for _, label := range runner.Labels {
		if label == nil {
			continue
		}
		present[strings.ToLower(label.GetName())] = struct{}{}
	}

	for _, want := range required {
		if _, ok := present[strings.ToLower(want)]; !ok {
			return false
		}
	}

	return true
}
