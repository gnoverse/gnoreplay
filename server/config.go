package main

import (
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	// Listen is the address of the HTTP server serving job pages and reports.
	Listen string `toml:"listen"`
	// PublicURL is how GitHub users reach this server; commit statuses link
	// to job pages built from it. Empty disables the links.
	PublicURL string `toml:"public_url"`
	// DataDir holds the job DB, git mirrors, job work dirs and reports.
	DataDir string `toml:"data_dir"`
	// Workers is the number of replays run concurrently.
	Workers int `toml:"workers"`

	GitHub GitHubConfig `toml:"github"`
	Chain  ChainConfig  `toml:"chain"`
	Job    JobConfig    `toml:"job"`

	// Rules select what is replayed and its priority: the first matching rule
	// wins, and its position is the priority (earlier runs first). Branches
	// and PRs that match no rule are ignored.
	Rules []Rule `toml:"rules"`
	// Repos lists per-repository settings, keyed by "owner/name".
	Repos map[string]RepoConfig `toml:"repos"`
}

type GitHubConfig struct {
	// TokenFile holds a GitHub token that can read the tracked repos'
	// contents and pull requests, and write their commit statuses.
	TokenFile string `toml:"token_file"`
	// StatusContext names the commit status posted on commits.
	StatusContext string `toml:"status_context"`
	// PollInterval is how often tracked branches and open PRs are polled.
	PollInterval Duration `toml:"poll_interval"`
	// GitURL is prefixed to "owner/name.git" to fetch commits.
	GitURL string `toml:"git_url"`
}

type ChainConfig struct {
	// GoldenDir is a stopped copy of the reference node's data directory
	// (at least db/blockstore.db and db/state.db). It is refreshed out of
	// band; each job takes its own copy of it.
	GoldenDir string `toml:"golden_dir"`
	// Genesis is the chain's genesis.json.
	Genesis string `toml:"genesis"`
}

type JobConfig struct {
	// GnoreplayOverlay is the gnoreplay source tree (gnoreplay/ in this
	// repo), copied into each checkout at contribs/gnoreplay and built there.
	GnoreplayOverlay string `toml:"gnoreplay_overlay"`
	// BuildSandbox and RunSandbox prefix the build and replay commands, e.g.
	// ["bwrap", "--unshare-net", ...] or ["msb", "run", ...]. PR code is
	// untrusted: at least the replay should run without network access or
	// credentials. "{job}" is substituted with the job ID and "{src}" with the
	// checkout (the replay's GNOROOT, which a VM sandbox must pass itself).
	BuildSandbox []string `toml:"build_sandbox"`
	RunSandbox   []string `toml:"run_sandbox"`
	// CleanupCommand runs after every job whatever its outcome, with the same
	// substitutions, e.g. to remove the job's sandbox VMs.
	CleanupCommand []string `toml:"cleanup_command"`
	// CopyCommand copies the golden DBs into a job dir; "{src}" and "{dst}"
	// are substituted. The default uses reflinks where the FS supports them.
	CopyCommand  []string `toml:"copy_command"`
	BuildTimeout Duration `toml:"build_timeout"`
	RunTimeout   Duration `toml:"run_timeout"`
	// KeepJobDirs keeps checkouts and DB copies after a job, for debugging.
	KeepJobDirs bool `toml:"keep_job_dirs"`
}

type Rule struct {
	Repo string `toml:"repo"` // "owner/name"
	// Event is "push" (commits on Branch) or "pull_request" (open PRs whose
	// base is Branch).
	Event  string `toml:"event"`
	Branch string `toml:"branch"`
}

type RepoConfig struct {
	// PublicReports serves this repo's job pages and reports to anyone, and
	// lists its jobs on /queue. Otherwise a job's page and report need the
	// job's secret, which only its commit status links to: keep false for
	// private repos.
	PublicReports bool `toml:"public_reports"`
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// defaultRules is the agreed priority order.
var defaultRules = []Rule{
	{Repo: "gnolang/gno", Event: eventPush, Branch: "chain/mainnet"},
	{Repo: "gnolang/gno", Event: eventPullRequest, Branch: "chain/mainnet"},
	{Repo: "gnolang/gno", Event: eventPush, Branch: "master"},
	{Repo: "gnolang/gno-fixes", Event: eventPush, Branch: "develop"},
	{Repo: "gnolang/gno", Event: eventPullRequest, Branch: "master"},
	{Repo: "gnolang/gno-fixes", Event: eventPullRequest, Branch: "develop"},
}

const (
	eventPush        = "push"
	eventPullRequest = "pull_request"
)

func loadConfig(path string) (*Config, error) {
	bz, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := toml.Unmarshal(bz, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.setDefaults()
	return cfg, cfg.validate()
}

func (c *Config) setDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Workers == 0 {
		c.Workers = 1
	}
	if c.GitHub.GitURL == "" {
		c.GitHub.GitURL = "https://github.com/"
	}
	if c.GitHub.StatusContext == "" {
		c.GitHub.StatusContext = "mainnet-replay"
	}
	if c.GitHub.PollInterval.Duration == 0 {
		c.GitHub.PollInterval.Duration = 2 * time.Minute
	}
	if len(c.Rules) == 0 {
		c.Rules = defaultRules
	}
	if len(c.Job.CopyCommand) == 0 {
		c.Job.CopyCommand = []string{"cp", "-a", "--reflink=auto", "{src}", "{dst}"}
	}
	if c.Job.BuildTimeout.Duration == 0 {
		c.Job.BuildTimeout.Duration = 20 * time.Minute
	}
	if c.Job.RunTimeout.Duration == 0 {
		c.Job.RunTimeout.Duration = 4 * time.Hour
	}
}

func (c *Config) validate() error {
	switch {
	case c.DataDir == "":
		return fmt.Errorf("data_dir is required")
	case c.Chain.GoldenDir == "":
		return fmt.Errorf("chain.golden_dir is required")
	case c.Chain.Genesis == "":
		return fmt.Errorf("chain.genesis is required")
	case c.GitHub.TokenFile == "":
		return fmt.Errorf("github.token_file is required")
	case c.Job.GnoreplayOverlay == "":
		return fmt.Errorf("job.gnoreplay_overlay is required")
	}
	for i, r := range c.Rules {
		if r.Event != eventPush && r.Event != eventPullRequest {
			return fmt.Errorf("rules[%d]: event must be %q or %q", i, eventPush, eventPullRequest)
		}
		if r.Repo == "" || r.Branch == "" {
			return fmt.Errorf("rules[%d]: repo and branch are required", i)
		}
	}
	return nil
}

// repos lists the repos the rules track, in rule order.
func (c *Config) repos() []string {
	var out []string
	for _, r := range c.Rules {
		if !slices.Contains(out, r.Repo) {
			out = append(out, r.Repo)
		}
	}
	return out
}

// priority returns the priority for an event (lower runs first), and false
// if no rule matches it.
func (c *Config) priority(repo, event, branch string) (int, bool) {
	for i, r := range c.Rules {
		if r.Repo == repo && r.Event == event && r.Branch == branch {
			return i + 1, true
		}
	}
	return 0, false
}
