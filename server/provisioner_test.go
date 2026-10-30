package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Workers are created with no public IP (the account's are few, and shared),
// in the private network.
func TestHetznerCreateNoPublicIP(t *testing.T) {
	var req struct {
		PublicNet *struct {
			EnableIPv4 bool `json:"enable_ipv4"`
			EnableIPv6 bool `json:"enable_ipv6"`
		} `json:"public_net"`
		Networks []int64 `json:"networks"`
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/servers" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"server": {"id": 42, "name": "gnoreplay-worker-x"}, "action": {"id": 1}}`)
	}))
	defer api.Close()

	p := &hetznerProvisioner{
		c:   hcloud.NewClient(hcloud.WithEndpoint(api.URL), hcloud.WithToken("token")),
		cfg: &HetznerConfig{ServerType: "ccx13", Image: "ubuntu-24.04", Location: "hel1", Network: 7},
	}
	m, err := p.Create(context.Background(), "gnoreplay-worker-x", "#!/bin/bash\n", nil)
	require.NoError(t, err)
	assert.Equal(t, "42", m.ID)
	require.NotNil(t, req.PublicNet, "public_net unset: the API would assign public IPs")
	assert.False(t, req.PublicNet.EnableIPv4)
	assert.False(t, req.PublicNet.EnableIPv6)
	assert.Equal(t, []int64{7}, req.Networks)
}
