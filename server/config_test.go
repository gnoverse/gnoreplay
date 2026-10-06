package main

import (
	"bytes"
	"os"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExampleConfig(t *testing.T) {
	bz, err := os.ReadFile("config.example.toml")
	require.NoError(t, err)
	// Strict: a key the config does not know is a stale example.
	var strict Config
	require.NoError(t, toml.NewDecoder(bytes.NewReader(bz)).DisallowUnknownFields().Decode(&strict))

	cfg, err := loadConfig("config.example.toml")
	require.NoError(t, err)
	assert.Equal(t, "/etc/gnoreplay/viewer-key", cfg.ViewerKeyFile)
	assert.Equal(t, defaultRules, cfg.Rules)
	assert.False(t, cfg.Repos["gnolang/gno-fixes"].PublicReports)
	assert.True(t, cfg.Repos["gnolang/gno"].PublicReports)
	assert.Equal(t, "4h0m0s", cfg.Job.RunTimeout.String())
}
