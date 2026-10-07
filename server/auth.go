package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Results of repos that aren't public are shown to the users their
// viewers_file lists, once signed in with GitHub. Sign-in only identifies the
// user: the OAuth app asks for no scopes. Who may see a repo is decided here,
// not asked of GitHub: its collaborator check needs push access, which the
// server's read-only token doesn't have. scripts/sync-viewers.sh writes the
// file from the repo's collaborators.

const (
	sessionCookie = "gnoreplay_session"
	oauthCookie   = "gnoreplay_oauth" // the pending sign-in: state and next page
	sessionTTL    = 7 * 24 * time.Hour
)

// oauthEndpoints are GitHub's; tests use a fake.
type oauthEndpoints struct{ authorize, token, user string }

var githubOAuth = oauthEndpoints{
	authorize: "https://github.com/login/oauth/authorize",
	token:     "https://github.com/login/oauth/access_token",
	user:      "https://api.github.com/user",
}

// Viewer is a signed-in GitHub user, as kept in the session cookie.
type Viewer struct {
	ID      int64  `json:"id"` // users are matched by ID: a login can be renamed, then taken
	Login   string `json:"login"`
	Expires int64  `json:"exp"` // unix seconds
}

func (s *Server) oauthEnabled() bool { return s.cfg.GitHub.OAuth != nil }

func (s *Server) secureCookies() bool { return strings.HasPrefix(s.cfg.PublicURL, "https://") }

// serveLogin starts a sign-in: GitHub sends the user back to serveCallback.
func (s *Server) serveLogin(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	state := rand.Text()
	http.SetCookie(w, &http.Cookie{
		Name: oauthCookie, Value: state + "|" + url.QueryEscape(localPath(r.URL.Query().Get("next"))),
		Path: "/auth/", MaxAge: 600, HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	q := url.Values{
		"client_id":    {s.cfg.GitHub.OAuth.ClientID},
		"redirect_uri": {strings.TrimRight(s.cfg.PublicURL, "/") + "/auth/callback"},
		"state":        {state},
		"allow_signup": {"false"},
	}
	http.Redirect(w, r, s.oauth.authorize+"?"+q.Encode(), http.StatusFound)
}

func (s *Server) serveCallback(w http.ResponseWriter, r *http.Request) {
	if !s.oauthEnabled() {
		http.NotFound(w, r)
		return
	}
	c, err := r.Cookie(oauthCookie)
	var state, next string
	if err == nil {
		var esc string
		state, esc, _ = strings.Cut(c.Value, "|")
		next, _ = url.QueryUnescape(esc)
	}
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Path: "/auth/", MaxAge: -1})
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "This sign-in expired or was not started here: sign in again.", http.StatusBadRequest)
		return
	}
	v, err := s.githubUser(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		s.logger.Error("sign-in", "err", err)
		http.Error(w, "GitHub sign-in failed.", http.StatusBadGateway)
		return
	}
	v.Expires = time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.signSession(v), Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
	s.logger.Info("signed in", "login", v.Login, "id", v.ID)
	http.Redirect(w, r, localPath(next), http.StatusFound)
}

func (s *Server) serveLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// localPath returns next if it is a path on this server, else "/": sign-in
// must not redirect elsewhere.
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

var oauthClient = &http.Client{Timeout: 20 * time.Second}

// githubUser exchanges a sign-in code for the user's identity. The token,
// which has no scopes, is used for that only, and dropped.
func (s *Server) githubUser(ctx context.Context, code string) (*Viewer, error) {
	form := url.Values{
		"client_id":     {s.cfg.GitHub.OAuth.ClientID},
		"client_secret": {s.oauthSecret},
		"code":          {code},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.oauth.token, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := doJSON(req, &tok); err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("exchange code: %q", tok.Error)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, s.oauth.user, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	v := &Viewer{}
	if err := doJSON(req, v); err != nil {
		return nil, fmt.Errorf("read user: %w", err)
	}
	if v.ID == 0 || v.Login == "" {
		return nil, errors.New("read user: no identity")
	}
	return v, nil
}

func doJSON(req *http.Request, out any) error {
	res, err := oauthClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	bz, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", res.Status)
	}
	return json.Unmarshal(bz, out)
}

// signSession encodes v as a cookie value: its JSON and an HMAC of it.
func (s *Server) signSession(v *Viewer) string {
	bz, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(bz)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload))
}

func (s *Server) mac(payload string) []byte {
	h := hmac.New(sha256.New, s.sessionKey)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

// viewer returns the request's signed-in user, or nil.
func (s *Server) viewer(r *http.Request) *Viewer {
	if len(s.sessionKey) == 0 {
		return nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return nil
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(payload)) {
		return nil
	}
	bz, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	v := &Viewer{}
	if json.Unmarshal(bz, v) != nil || time.Now().Unix() > v.Expires {
		return nil
	}
	return v
}

// loadSessionKey reads the key signing session cookies, creating it on the
// first start: keeping it lets sessions survive restarts.
func loadSessionKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil && len(key) >= 32 {
		return key, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	rand.Read(key)
	return key, os.WriteFile(path, key, 0o600)
}

// viewerLists caches the viewers files, reread when they change.
type viewerLists struct {
	mu    sync.Mutex
	files map[string]*viewerList
}

type viewerList struct {
	modTime time.Time
	size    int64
	ids     map[int64]bool
}

// allowed reports whether user id may see repo's results.
func (s *Server) allowed(repo string, id int64) bool {
	path := s.cfg.Repos[repo].ViewersFile
	if path == "" {
		return false
	}
	ids, err := s.viewers.load(path)
	if err != nil {
		s.logger.Error("read viewers file", "repo", repo, "err", err)
		return false
	}
	return ids[id]
}

func (vl *viewerLists) load(path string) (map[int64]bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	vl.mu.Lock()
	defer vl.mu.Unlock()
	if l := vl.files[path]; l != nil && l.modTime.Equal(info.ModTime()) && l.size == info.Size() {
		return l.ids, nil
	}
	ids, err := readViewers(path)
	if err != nil {
		return nil, err
	}
	if vl.files == nil {
		vl.files = map[string]*viewerList{}
	}
	vl.files[path] = &viewerList{modTime: info.ModTime(), size: info.Size(), ids: ids}
	return ids, nil
}

// readViewers parses a viewers file: a GitHub user ID per line, then
// anything (the login, for people reading it); # starts a comment.
func readViewers(path string) (map[int64]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ids := map[int64]bool{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: not a GitHub user ID: %q", path, n, fields[0])
		}
		ids[id] = true
	}
	return ids, sc.Err()
}
