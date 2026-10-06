package main

import (
	"bytes"
	"os"
	"testing"

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
	assert.Equal(t, "/etc/gnoreplay/viewer-key", cfg.ViewerKeyFile)
	assert.Equal(t, defaultRules, cfg.Rules)
	assert.False(t, cfg.Repos["gnolang/gno-fixes"].PublicReports)
	assert.True(t, cfg.Repos["gnolang/gno"].PublicReports)
	assert.Equal(t, "4h0m0s", cfg.Job.RunTimeout.String())
	assert.Nil(t, cfg.DigitalOcean)
}

func TestDigitalOceanExampleConfig(t *testing.T) {
	cfg := loadExample(t, "config.digitalocean.example.toml")
	require.NotNil(t, cfg.DigitalOcean)
	assert.Equal(t, "m-2vcpu-16gb", cfg.DigitalOcean.Size)
	assert.Equal(t, "gnoreplay-worker", cfg.DigitalOcean.Tag)
	assert.Equal(t, defaultRules, cfg.Rules)

	// Workers must get a VPC of their own and an SSH key.
	cfg.DigitalOcean.VPCUUID = ""
	assert.ErrorContains(t, cfg.validate(), "vpc_uuid")
	cfg.DigitalOcean.VPCUUID = "x"
	cfg.DigitalOcean.SSHKeys = nil
	assert.ErrorContains(t, cfg.validate(), "ssh_keys")
}
