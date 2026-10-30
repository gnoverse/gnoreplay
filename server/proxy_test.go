package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Workers' HTTPS goes through the worker endpoint, as Go's does with
// https_proxy set, to the allowed hosts only.
func TestWorkerProxy(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "a module")
	}))
	defer upstream.Close()
	allowed := strings.TrimPrefix(upstream.URL, "https://")
	proxyHosts[allowed] = true
	t.Cleanup(func() { delete(proxyHosts, allowed) })

	srv := newServer(&Config{}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	endpoint := httptest.NewServer(srv.workerRoutes())
	defer endpoint.Close()
	proxy, err := url.Parse(endpoint.URL)
	require.NoError(t, err)
	client := upstream.Client()
	client.Transport.(*http.Transport).Proxy = http.ProxyURL(proxy)

	resp, err := client.Get(upstream.URL + "/github.com/pkg/errors/@v/v0.9.1.zip")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, "a module", string(body))

	// Anything else is refused, before any connection to it.
	for _, target := range []string{"https://example.com/", "https://go.dev:8443/"} {
		_, err := client.Get(target)
		assert.ErrorContains(t, err, "Forbidden", target)
	}
}
