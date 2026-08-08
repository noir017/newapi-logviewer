package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Auth is delegated to new-api itself.
//
// AUTH_MODE=bearer requires `Authorization: Bearer <token>` and validates it
// against new-api's /api/user/self. Cookie auth is deliberately absent: new-api
// authenticates dashboard calls with an access token its SPA keeps in memory
// only, so a browser cannot hand it to us. The refresh-cookie exchange that
// would work is rate-limited (CriticalRateLimit, 20 calls / 20 min per IP) and
// rotates the user's cookie on every call, which is not worth the fragility.
//
// AUTH_MODE=none is open access - LAN only, since anyone who can reach the URL
// reads every prompt.

type authUser struct {
	Username string `json:"username"`
	Role     int    `json:"role"`
}

type authVerdict struct {
	ok     bool
	user   authUser
	reason string
	exp    time.Time
}

type authCache struct {
	mu sync.Mutex
	m  map[string]authVerdict
}

func newAuthCache() *authCache { return &authCache{m: map[string]authVerdict{}} }

func (c *authCache) get(k string) (authVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	if !ok || time.Now().After(v.exp) {
		return authVerdict{}, false
	}
	return v, true
}

func (c *authCache) put(k string, v authVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) > 512 {
		c.m = map[string]authVerdict{}
	}
	c.m[k] = v
}

type authenticator struct {
	mode        string
	newapiURL   string
	requireAdmn bool
	ttl         time.Duration
	cache       *authCache
	client      *http.Client
}

func newAuthenticator(cfg Config) *authenticator {
	return &authenticator{
		mode:        cfg.AuthMode,
		newapiURL:   strings.TrimRight(cfg.NewAPIURL, "/"),
		requireAdmn: cfg.RequireAdmin,
		ttl:         cfg.AuthTTL,
		cache:       newAuthCache(),
		client:      &http.Client{Timeout: 8 * time.Second},
	}
}

// check returns (ok, user, reason). The token is hashed before it is used as a
// cache key so a memory dump of this process does not hand over credentials.
func (a *authenticator) check(r *http.Request) (bool, authUser, string) {
	if a.mode == "none" {
		return true, authUser{Username: "-"}, ""
	}
	tok := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(tok), "bearer ") {
		return false, authUser{}, "需要 Authorization: Bearer <access_token>"
	}

	sum := sha256.Sum256([]byte(tok))
	key := hex.EncodeToString(sum[:])
	if v, ok := a.cache.get(key); ok {
		return v.ok, v.user, v.reason
	}

	ok, user, reason := a.verify(tok)
	// Negative verdicts are cached too, briefly: a browser tab polling with a
	// stale token must not turn into a request flood against new-api.
	ttl := a.ttl
	if !ok {
		ttl = 15 * time.Second
	}
	a.cache.put(key, authVerdict{ok: ok, user: user, reason: reason, exp: time.Now().Add(ttl)})
	return ok, user, reason
}

func (a *authenticator) verify(token string) (bool, authUser, string) {
	req, err := http.NewRequest("GET", a.newapiURL+"/api/user/self", nil)
	if err != nil {
		return false, authUser{}, "auth request build failed"
	}
	req.Header.Set("Authorization", token)
	res, err := a.client.Do(req)
	if err != nil {
		return false, authUser{}, "token auth failed: " + err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != 200 {
		return false, authUser{}, "无效的 token（New API 返回 " + res.Status + "）"
	}
	var payload struct {
		Success bool     `json:"success"`
		Message string   `json:"message"`
		Data    authUser `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false, authUser{}, "auth response not JSON"
	}
	if !payload.Success || payload.Data.Username == "" {
		msg := payload.Message
		if msg == "" {
			msg = "invalid token"
		}
		return false, authUser{}, msg
	}
	if a.requireAdmn && payload.Data.Role < 100 {
		return false, payload.Data, "需要管理员权限（role >= 100）"
	}
	return true, payload.Data, ""
}
