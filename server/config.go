package main

import (
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	// Listen is the address of the HTTP server serving the results.
	Listen string `toml:"listen"`
	// PublicURL is how users reach this server; reports link to each other
	// with it. Empty disables the links.
	PublicURL string `toml:"public_url"`
	// ViewerKeyFile holds a key that unlocks the results of repos without
	// public_reports (pass it once as ?key=, it is then kept in a cookie).
	// Without it, those results are not served at all.
	ViewerKeyFile string `toml:"viewer_key_file"`
	// DataDir holds the job DB, git mirrors, job work dirs and reports.
	DataDir string `toml:"data_dir"`
	// Workers is the number of replays run concurrently: local sandboxes, or
	// worker droplets with [digitalocean].
	Workers int `toml:"workers"`

	GitHub GitHubConfig `toml:"github"`
	Chain  ChainConfig  `toml:"chain"`
	Job    JobConfig    `toml:"job"`
	// DigitalOcean, when set, runs each replay on its own worker droplet
	// instead of in a local sandbox.
	DigitalOcean *DigitalOceanConfig `toml:"digitalocean"`

	// Rules select what is replayed and its priority: the first matching rule
	// wins, and its position is the priority (earlier runs first). Branches
	// and PRs that match no rule are ignored.
	Rules []Rule `toml:"rules"`
	// Repos lists per-repository settings, keyed by "owner/name".
	Repos map[string]RepoConfig `toml:"repos"`
}

type GitHubConfig struct {
	// TokenFile holds a GitHub token that can read the tracked repos'
	// contents and pull requests. The server never writes to GitHub, so the
	// token should not be able to either.
	TokenFile string `toml:"token_file"`
	// PollInterval is how often tracked branches and open PRs are polled.
	PollInterval Duration `toml:"poll_interval"`
	// GitURL is prefixed to "owner/name.git" to fetch commits.
	GitURL string `toml:"git_url"`
}

// DigitalOceanConfig runs replays on disposable droplets: the droplet is the
// sandbox. Each gets a startup script that downloads the job's inputs from the
// coordinator over the private network, replays, and posts back the report;
// the coordinator then deletes it.
type DigitalOceanConfig struct {
	// TokenFile holds an API token that can create, read and delete droplets.
	TokenFile string `toml:"token_file"`
	Region    string `toml:"region"`
	Size      string `toml:"size"`
	Image     string `toml:"image"`
	// VPCUUID is the private network workers join. Use a VPC of their own:
	// PR code runs on them and can reach anything else in it.
	VPCUUID string `toml:"vpc_uuid"`
	// SSHKeys are key fingerprints (or IDs) set on workers. One is required:
	// without it, DigitalOcean emails a root password for every droplet.
	SSHKeys []string `toml:"ssh_keys"`
	// Tag marks the worker droplets, for firewalls and cleanup.
	Tag string `toml:"tag"`
	// Listen is the coordinator's private address that workers download
	// their inputs from, and URL how they reach it ("http://10.x.y.z:8081").
	Listen string `toml:"listen"`
	URL    string `toml:"url"`
	// GoVersion is the Go toolchain installed on workers (newer versions
	// required by a tree are fetched automatically).
	GoVersion string `toml:"go_version"`
	// GoMemLimit is the replay's GOMEMLIMIT on the worker.
	GoMemLimit string `toml:"go_mem_limit"`
	// MaxAge is how long a worker may exist, from its creation, whatever
	// its job's state: older ones are deleted (and their job cancelled).
	// job.run_timeout must leave room for the worker's boot and build.
	MaxAge Duration `toml:"max_age"`
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
	// PublicReports shows this repo's results to anyone. Otherwise they need
	// the viewer key: keep false for private repos.
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
		if c.DigitalOcean != nil {
			// Below the workers' max age, so jobs time out before their
			// worker is deleted from under them.
			c.Job.RunTimeout.Duration = 3*time.Hour + 30*time.Minute
		}
	}
	if do := c.DigitalOcean; do != nil {
		if do.MaxAge.Duration == 0 {
			do.MaxAge.Duration = 4 * time.Hour
		}
		if do.Size == "" {
			do.Size = "m-2vcpu-16gb"
		}
		if do.Image == "" {
			do.Image = "ubuntu-24-04-x64"
		}
		if do.Tag == "" {
			do.Tag = "gnoreplay-worker"
		}
		if do.GoVersion == "" {
			do.GoVersion = "1.26.1"
		}
		if do.GoMemLimit == "" {
			do.GoMemLimit = "12GiB"
		}
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
	if do := c.DigitalOcean; do != nil {
		switch {
		case do.TokenFile == "" || do.Region == "":
			return fmt.Errorf("digitalocean.token_file and digitalocean.region are required")
		case do.VPCUUID == "":
			return fmt.Errorf("digitalocean.vpc_uuid is required: workers must not share a private network")
		case len(do.SSHKeys) == 0:
			return fmt.Errorf("digitalocean.ssh_keys is required (otherwise every droplet's root password is emailed)")
		case do.Listen == "" || do.URL == "":
			return fmt.Errorf("digitalocean.listen and digitalocean.url are required")
		case c.Job.RunTimeout.Duration >= do.MaxAge.Duration:
			return fmt.Errorf("job.run_timeout (%s) must be below digitalocean.max_age (%s)", c.Job.RunTimeout, do.MaxAge)
		}
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
