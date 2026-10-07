package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOAuth stands in for GitHub's OAuth endpoints: a sign-in code is the key
// of the user it signs in.
func fakeOAuth(t *testing.T, users map[string]Viewer) oauthEndpoints {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "cid", r.Form.Get("client_id"))
		assert.Equal(t, "secret", r.Form.Get("client_secret"))
		code := r.Form.Get("code")
		if _, ok := users[code]; !ok {
			json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + code})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		code, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		u, known := users[code]
		if !ok || !known {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(u)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return oauthEndpoints{authorize: srv.URL + "/authorize", token: srv.URL + "/token", user: srv.URL + "/user"}
}

// enableSignIn turns on sign-in with GitHub on srv, against fakeOAuth.
func enableSignIn(t *testing.T, srv *Server, users map[string]Viewer) {
	t.Helper()
	srv.cfg.GitHub.OAuth = &OAuthConfig{ClientID: "cid", ClientSecretFile: "unused"}
	srv.oauthSecret = "secret"
	srv.sessionKey = []byte("0123456789abcdef0123456789abcdef")
	srv.oauth = fakeOAuth(t, users)
}

// signIn goes through sign-in with the code (as GitHub would send the user
// back), and returns where the server then sends the browser.
func signIn(t *testing.T, client *http.Client, base, code, next string) (int, string) {
	t.Helper()
	noFollow := *client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := noFollow.Get(base + "/login?next=" + url.QueryEscape(next))
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, http.StatusFound, res.StatusCode)
	authorize, err := url.Parse(res.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "cid", authorize.Query().Get("client_id"))
	assert.Empty(t, authorize.Query().Get("scope"), "sign-in asks for no scopes")
	assert.Equal(t, "https://replay.example/auth/callback", authorize.Query().Get("redirect_uri"))

	q := url.Values{"code": {code}, "state": {authorize.Query().Get("state")}}
	res, err = noFollow.Get(base + "/auth/callback?" + q.Encode())
	require.NoError(t, err)
	res.Body.Close()
	return res.StatusCode, res.Header.Get("Location")
}

func newJar(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{Jar: jar}
}

func TestSignIn(t *testing.T) {
	viewers := filepath.Join(t.TempDir(), "viewers")
	require.NoError(t, os.WriteFile(viewers, []byte("# gnolang/gno-fixes\n42 alice\n"), 0o644))
	cfg := &Config{
		DataDir: t.TempDir(), PublicURL: "https://replay.example",
		Repos: map[string]RepoConfig{"gnolang/gno-fixes": {ViewersFile: viewers}},
	}
	cfg.setDefaults()
	q := newTestQueue(t)
	srv := newServer(cfg, q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableSignIn(t, srv, map[string]Viewer{
		"alice-code": {ID: 42, Login: "alice"},
		"bob-code":   {ID: 7, Login: "bob"},
		// A user who took the login of a listed one, renamed since: users
		// are matched by ID.
		"mallory-code": {ID: 666, Login: "alice"},
	})
	require.NoError(t, srv.enqueueFor("gnolang/gno-fixes", eventPush, "develop", 0, "abc"))
	httpSrv := httptest.NewServer(srv.routes())
	defer httpSrv.Close()
	status := func(client *http.Client, path string) int {
		t.Helper()
		res, err := client.Get(httpSrv.URL + path)
		require.NoError(t, err)
		res.Body.Close()
		return res.StatusCode
	}

	// Signed out: private results are not served.
	anon := newJar(t)
	assert.Equal(t, http.StatusNotFound, status(anon, "/jobs/1"))
	res, err := anon.Get(httpSrv.URL + "/")
	require.NoError(t, err)
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	assert.Contains(t, string(page), `<a href="/login?next=%2f">Sign in with GitHub</a>`)

	// A listed user signs in, and is sent back where they were.
	alice := newJar(t)
	code, next := signIn(t, alice, httpSrv.URL, "alice-code", "/jobs/1")
	require.Equal(t, http.StatusFound, code)
	assert.Equal(t, "/jobs/1", next)
	assert.Equal(t, http.StatusOK, status(alice, "/jobs/1"))
	res, err = alice.Get(httpSrv.URL + "/")
	require.NoError(t, err)
	page, _ = io.ReadAll(res.Body)
	res.Body.Close()
	assert.Contains(t, string(page), "@alice")
	assert.Contains(t, string(page), "gnolang/gno-fixes")

	// Others sign in, but don't see it.
	for _, user := range []string{"bob-code", "mallory-code"} {
		c := newJar(t)
		code, _ := signIn(t, c, httpSrv.URL, user, "/")
		require.Equal(t, http.StatusFound, code)
		assert.Equal(t, http.StatusNotFound, status(c, "/jobs/1"), user)
	}

	// The viewers file is reread when it changes.
	bob := newJar(t)
	signIn(t, bob, httpSrv.URL, "bob-code", "/")
	require.NoError(t, os.WriteFile(viewers, []byte("42 alice\n7 bob\n"), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(viewers, future, future))
	assert.Equal(t, http.StatusOK, status(bob, "/jobs/1"))

	// Signing out ends it.
	res, err = alice.Post(httpSrv.URL+"/logout", "", nil)
	require.NoError(t, err)
	res.Body.Close()
	assert.Equal(t, http.StatusNotFound, status(alice, "/jobs/1"))

	// Sign-in only sends back to this server.
	for _, next := range []string{"//evil.example/x", "https://evil.example", "/\\evil.example"} {
		c := newJar(t)
		code, loc := signIn(t, c, httpSrv.URL, "alice-code", next)
		require.Equal(t, http.StatusFound, code)
		assert.Equal(t, "/", loc, next)
	}

	// A bad code fails; so does a callback this browser didn't start.
	code, _ = signIn(t, newJar(t), httpSrv.URL, "wrong-code", "/")
	assert.Equal(t, http.StatusBadGateway, code)
	assert.Equal(t, http.StatusBadRequest, status(newJar(t), "/auth/callback?code=alice-code&state=forged"))

	// A session cookie can't be kept past its expiry, or altered.
	withSession := func(value string) *http.Client {
		c := newJar(t)
		u, _ := url.Parse(httpSrv.URL)
		c.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: value, Path: "/"}})
		return c
	}
	hour := time.Now().Add(time.Hour).Unix()
	assert.Equal(t, http.StatusOK, status(withSession(srv.signSession(&Viewer{ID: 42, Login: "alice", Expires: hour})), "/jobs/1"))
	expired := srv.signSession(&Viewer{ID: 42, Login: "alice", Expires: time.Now().Add(-time.Hour).Unix()})
	assert.Equal(t, http.StatusNotFound, status(withSession(expired), "/jobs/1"))
	alicePayload, _, _ := strings.Cut(srv.signSession(&Viewer{ID: 42, Login: "alice", Expires: hour}), ".")
	_, malloryMAC, _ := strings.Cut(srv.signSession(&Viewer{ID: 666, Login: "mallory", Expires: hour}), ".")
	assert.Equal(t, http.StatusNotFound, status(withSession(alicePayload+"."+malloryMAC), "/jobs/1"))
}

// Without sign-in configured, private results are not served at all.
func TestNoSignIn(t *testing.T) {
	cfg := &Config{DataDir: t.TempDir()}
	cfg.setDefaults()
	q := newTestQueue(t)
	srv := newServer(cfg, q, newFakeGitHub(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, srv.enqueueFor("gnolang/gno-fixes", eventPush, "develop", 0, "abc"))
	for _, path := range []string{"/jobs/1", "/gnolang/gno-fixes/tree/develop", "/login", "/auth/callback"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
	}
}

func TestReadViewers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "viewers")
	require.NoError(t, os.WriteFile(path, []byte("# who\n42 alice\n\n  7\tbob # a comment\n"), 0o644))
	ids, err := readViewers(path)
	require.NoError(t, err)
	assert.Equal(t, map[int64]bool{42: true, 7: true}, ids)
	require.NoError(t, os.WriteFile(path, []byte("alice\n"), 0o644))
	_, err = readViewers(path)
	assert.ErrorContains(t, err, "not a GitHub user ID")
}
