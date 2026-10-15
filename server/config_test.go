package main

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadExample loads an example config, failing on keys the config does not
// know: they would be a stale example.
func loadExample(t *testing.T, path string) *Config {
	t.Helper()
	bz, err := os.ReadFile(path)
	require.NoError(t, err)
	var strict Config
	require.NoError(t, toml.NewDecoder(bytes.NewReader(bz)).DisallowUnknownFields().Decode(&strict))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	return cfg
}

func TestExampleConfig(t *testing.T) {
	cfg := loadExample(t, "config.example.toml")
	require.NotNil(t, cfg.GitHub.OAuth)
	assert.Equal(t, "/etc/gnoreplay/github-oauth-secret", cfg.GitHub.OAuth.ClientSecretFile)
	assert.Equal(t, defaultRules, cfg.Rules)
	assert.False(t, cfg.Repos["gnolang/gno-fixes"].PublicReports)
	assert.Equal(t, "/etc/gnoreplay/gno-fixes-viewers", cfg.Repos["gnolang/gno-fixes"].ViewersFile)

	// Viewers sign in with GitHub, whose callback is on the public URL.
	oauth := cfg.GitHub.OAuth
	cfg.PublicURL = ""
	assert.ErrorContains(t, cfg.validate(), "github.oauth needs public_url")
	cfg.PublicURL = "https://replay.example.org"
	cfg.GitHub.OAuth = nil
	assert.ErrorContains(t, cfg.validate(), "viewers_file needs github.oauth")
	cfg.GitHub.OAuth = oauth
	assert.True(t, cfg.Repos["gnolang/gno"].PublicReports)
	assert.Equal(t, "4h0m0s", cfg.Job.RunTimeout.String())
	assert.Nil(t, cfg.Hetzner)

	// A branch is pushed to by name.
	cfg.Rules = append(cfg.Rules, Rule{Repo: "gnolang/gno", Event: eventPush, Branch: anyBranch})
	assert.ErrorContains(t, cfg.validate(), "only for pull_request rules")
}

func TestHetznerExampleConfig(t *testing.T) {
	cfg := loadExample(t, "config.hetzner.example.toml")
	require.NotNil(t, cfg.Hetzner)
	assert.Equal(t, "cpx42", cfg.Hetzner.ServerType)
	assert.Equal(t, "ubuntu-24.04", cfg.Hetzner.Image)
	assert.Equal(t, []int64{12345678}, cfg.Hetzner.SSHKeys)
	assert.Equal(t, defaultRules, cfg.Rules)

	assert.Equal(t, "4h0m0s", cfg.Hetzner.MaxAge.String())

	// A job must time out before its worker is deleted from under it.
	cfg.Job.RunTimeout = cfg.Hetzner.MaxAge
	assert.ErrorContains(t, cfg.validate(), "must be below hetzner.max_age")
	cfg.Job.RunTimeout.Duration = time.Hour

	// The watchdog scripts delete a worker on one job after 4h, whatever the
	// server's own limits allow.
	cfg.Hetzner.MaxAge.Duration = 5 * time.Hour
	assert.ErrorContains(t, cfg.validate(), "must not exceed")
	cfg.Hetzner.MaxAge.Duration = 4 * time.Hour

	// Workers must get a network of their own and an SSH key.
	cfg.Hetzner.Network = 0
	assert.ErrorContains(t, cfg.validate(), "hetzner.network")
	cfg.Hetzner.Network = 1
	cfg.Hetzner.SSHKeys = nil
	assert.ErrorContains(t, cfg.validate(), "ssh_keys")
}
