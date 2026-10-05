package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExampleConfig(t *testing.T) {
	cfg, err := loadConfig("config.example.toml")
	require.NoError(t, err)
	assert.Equal(t, defaultRules, cfg.Rules)
	assert.False(t, cfg.Repos["gnolang/gno-fixes"].PublicReports)
	assert.True(t, cfg.Repos["gnolang/gno"].PublicReports)
	assert.Equal(t, "4h0m0s", cfg.Job.RunTimeout.String())
}
