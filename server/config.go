package main

import (
	"bytes"
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
	// DataDir holds the job DB, git mirrors, job work dirs and reports.
	DataDir string `toml:"data_dir"`
	// Workers is the number of replays run concurrently: local sandboxes, or
	// worker servers with [hetzner].
	Workers int `toml:"workers"`

	GitHub GitHubConfig `toml:"github"`
	Chain  ChainConfig  `toml:"chain"`
	Job    JobConfig    `toml:"job"`
	// Hetzner, when set, runs each replay on a worker server of its own
	// instead of in a local sandbox.
	Hetzner *HetznerConfig `toml:"hetzner"`

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
	// OAuth lets users sign in with GitHub, to see the results of repos that
	// aren't public (see RepoConfig.ViewersFile). Without it, those are not
	// served at all.
	OAuth *OAuthConfig `toml:"oauth"`
}

// OAuthConfig is a GitHub OAuth app (not a GitHub App: it needs no approval
// from the repos' organization), with <public_url>/auth/callback as its
// callback URL. It asks for no scopes: users only prove who they are.
type OAuthConfig struct {
	ClientID         string `toml:"client_id"`
	ClientSecretFile string `toml:"client_secret_file"`
}

// HetznerConfig runs replays on Hetzner Cloud servers: the server is the
// sandbox, reinstalled from scratch for each job, with a startup script that
// downloads the job's inputs from the coordinator over the private network,
// replays, and posts back the report. Hetzner bills started hours from a
// server's creation: a server that finished a job runs the next one, and one
// left idle is deleted at the end of its billing hour.
type HetznerConfig struct {
	// TokenFile holds an API token (read & write) of the project workers are
	// created in.
	TokenFile  string `toml:"token_file"`
	Location   string `toml:"location"`
	ServerType string `toml:"server_type"`
	Image      string `toml:"image"`
	// Network is the ID of the private network workers join. Use one of
	// their own: PR code runs on them and can reach anything else in it.
	Network int64 `toml:"network"`
	// SSHKeys are the IDs of SSH keys set on workers. One is required:
	// without it, Hetzner emails a root password for every server.
	SSHKeys []int64 `toml:"ssh_keys"`
	// Listen is the coordinator's private address that workers download
	// their inputs from (and Go and its modules, through it: they have no
	// public IP), and URL how they reach it ("http://10.x.y.z:8081").
	Listen string `toml:"listen"`
	URL    string `toml:"url"`
	// GoVersion is the Go toolchain installed on workers (newer versions
	// required by a tree are fetched automatically).
	GoVersion string `toml:"go_version"`
	// GoMemLimit is the replay's GOMEMLIMIT on the worker.
	GoMemLimit string `toml:"go_mem_limit"`
	// MaxAge is how long a worker may run one job, from when it was given
	// it, whatever the job's state: the worker is then deleted (and its job
	// cancelled). It may not exceed maxWorkerAge, the cap the watchdog
	// scripts enforce. job.run_timeout must leave room for the worker's
	// boot and build.
	MaxAge Duration `toml:"max_age"`
}

type ChainConfig struct {
	// GoldenDir is a copy of a mainnet node's data directory (at least
	// db/blockstore.db and db/state.db). It is refreshed out of band; each
	// job takes its own copy of it.
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
	// base is Branch, or any open PR if Branch is "*").
	Event  string `toml:"event"`
	Branch string `toml:"branch"`
}

type RepoConfig struct {
	// PublicReports shows this repo's results to anyone: keep false for
	// private repos.
	PublicReports bool `toml:"public_reports"`
	// ViewersFile lists who may see this repo's results when they aren't
	// public, once signed in with GitHub: a GitHub user ID per line, then
	// anything (e.g. the login); # starts a comment. It is reread when it
	// changes. scripts/sync-viewers.sh writes it from the repo's collaborators.
	ViewersFile string `toml:"viewers_file"`
}

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// maxWorkerAge is the cap the watchers enforce independently of the server:
// deploy/watchdog.sh and .github/workflows/worker-watchdog.yml delete a
// worker that has run one job for over 4h, whatever the server's own limits
// say.
const maxWorkerAge = 4 * time.Hour

// defaultRules is the agreed priority order.
var defaultRules = []Rule{
	{Repo: "gnolang/gno", Event: eventPush, Branch: "chain/mainnet"},
	{Repo: "gnolang/gno", Event: eventPullRequest, Branch: "chain/mainnet"},
	{Repo: "gnolang/gno", Event: eventPush, Branch: "master"},
	{Repo: "gnolang/gno-fixes", Event: eventPush, Branch: "develop"},
	{Repo: "gnolang/gno", Event: eventPullRequest, Branch: "master"},
	// Fixes are also developed against other bases, e.g. master.
	{Repo: "gnolang/gno-fixes", Event: eventPullRequest, Branch: anyBranch},
}

const (
	eventPush        = "push"
	eventPullRequest = "pull_request"
	// anyBranch as a pull_request rule's branch matches PRs into any branch.
	anyBranch = "*"
)

// loadConfig reads a config file. Unknown keys are refused: a typo must not
// silently change the mode the server runs in (e.g. [hetzners] leaving it
// local, or a misplaced max_age).
func loadConfig(path string) (*Config, error) {
	bz, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := toml.NewDecoder(bytes.NewReader(bz)).DisallowUnknownFields().Decode(cfg); err != nil {
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
		if c.Hetzner != nil {
			// Below the workers' max age, so jobs time out before their
			// worker is deleted from under them.
			c.Job.RunTimeout.Duration = 3*time.Hour + 30*time.Minute
		}
	}
	if h := c.Hetzner; h != nil {
		if h.MaxAge.Duration == 0 {
			h.MaxAge.Duration = 4 * time.Hour
		}
		if h.Image == "" {
			h.Image = "ubuntu-24.04"
		}
		if h.GoVersion == "" {
			h.GoVersion = "1.26.1"
		}
		if h.GoMemLimit == "" {
			h.GoMemLimit = "12GiB"
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
	if o := c.GitHub.OAuth; o != nil {
		switch {
		case o.ClientID == "" || o.ClientSecretFile == "":
			return fmt.Errorf("github.oauth.client_id and github.oauth.client_secret_file are required")
		case c.PublicURL == "":
			return fmt.Errorf("github.oauth needs public_url, for its callback URL")
		}
	}
	for name, repo := range c.Repos {
		if repo.ViewersFile != "" && c.GitHub.OAuth == nil {
			return fmt.Errorf("repos.%q.viewers_file needs github.oauth to sign viewers in", name)
		}
	}
	if h := c.Hetzner; h != nil {
		switch {
		case h.TokenFile == "" || h.Location == "" || h.ServerType == "":
			return fmt.Errorf("hetzner.token_file, hetzner.location and hetzner.server_type are required")
		case h.Network == 0:
			return fmt.Errorf("hetzner.network is required: workers must not share a private network")
		case len(h.SSHKeys) == 0:
			return fmt.Errorf("hetzner.ssh_keys is required (otherwise every server's root password is emailed)")
		case h.Listen == "" || h.URL == "":
			return fmt.Errorf("hetzner.listen and hetzner.url are required")
		case h.MaxAge.Duration > maxWorkerAge:
			return fmt.Errorf("hetzner.max_age (%s) must not exceed %s: the watchdog scripts delete a worker after that, whatever the server allows", h.MaxAge, maxWorkerAge)
		case c.Job.RunTimeout.Duration >= h.MaxAge.Duration:
			return fmt.Errorf("job.run_timeout (%s) must be below hetzner.max_age (%s)", c.Job.RunTimeout, h.MaxAge)
		}
	}
	for i, r := range c.Rules {
		if r.Event != eventPush && r.Event != eventPullRequest {
			return fmt.Errorf("rules[%d]: event must be %q or %q", i, eventPush, eventPullRequest)
		}
		if r.Repo == "" || r.Branch == "" {
			return fmt.Errorf("rules[%d]: repo and branch are required", i)
		}
		if r.Branch == anyBranch && r.Event != eventPullRequest {
			return fmt.Errorf("rules[%d]: branch %q is only for pull_request rules", i, anyBranch)
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
		if r.Repo == repo && r.Event == event && (r.Branch == branch || r.Branch == anyBranch) {
			return i + 1, true
		}
	}
	return 0, false
}
