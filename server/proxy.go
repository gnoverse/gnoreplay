package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// proxyHosts are the hosts the worker endpoint tunnels workers' HTTPS to.
// Workers have no public IP (the account's are few, and shared): this proxy is
// all of the internet they reach, which PR code, running there as root, only
// needs for Go and its modules.
var proxyHosts = map[string]bool{
	"go.dev:443":           true, // the toolchain, from go.dev/dl, which redirects to dl.google.com
	"dl.google.com:443":    true,
	"proxy.golang.org:443": true, // modules, and the toolchain a tree may ask for
	"sum.golang.org:443":   true,
	// Where proxy.golang.org redirects some downloads (toolchains).
	"storage.googleapis.com:443": true,
}

// proxyConnect tunnels a worker's CONNECT request to an allowed host.
func (s *Server) proxyConnect(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if !proxyHosts[host] {
		s.logger.Warn("worker proxy: host not allowed", "host", host, "from", r.RemoteAddr)
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	upstream, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(r.Context(), "tcp", host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	// The server's header timeout must not cut the tunnel.
	conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	// Either side closing ends the tunnel: TLS needs no half-close.
	go func() {
		io.Copy(upstream, buf.Reader) // what the worker sent past its request, then the rest
		upstream.Close()
	}()
	io.Copy(conn, upstream)
}
