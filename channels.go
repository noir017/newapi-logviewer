package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// channelResolver turns the channel id in a log line into the channel's name.
//
// New API never writes the name to the log. The consume line carries only
// `"channel_id":6`, and `fullRequestURL` gives the upstream address - so the
// address is the only label recoverable from files alone.
//
// That is usually not enough to tell channels apart. A gateway commonly has a
// dozen keys pointed at the same host (measured here: 22 of 33 channels share
// `integrate.api.nvidia.com`), and they differ only by name. So the name is
// fetched from New API's own admin API and cached.
//
// This is the one place the viewer talks to anything other than files, and it
// is entirely optional: with no token configured the list falls back to the
// upstream host, which costs no network at all.
type channelResolver struct {
	base   string
	token  string
	ttl    time.Duration
	client *http.Client

	fetchMu sync.Mutex // serialises the HTTP call, held across it

	mu      sync.Mutex // guards the fields below, never held during I/O
	names   map[int]string
	fetched time.Time
	tried   time.Time // last attempt, successful or not; throttles retries
}

const (
	channelTTL        = 5 * time.Minute
	channelRetryEvery = 30 * time.Second
	channelPageSize   = 100
	channelMaxPages   = 20
)

func newChannelResolver(base, token string) *channelResolver {
	return &channelResolver{
		base:   strings.TrimRight(base, "/"),
		token:  token,
		ttl:    channelTTL,
		client: &http.Client{Timeout: 5 * time.Second},
		names:  map[int]string{},
	}
}

// enabled reports whether resolution is even possible. A nil receiver is
// allowed so tests and the reingest path can leave it unset.
func (c *channelResolver) enabled() bool {
	return c != nil && c.base != "" && c.token != ""
}

// name returns the channel's display name, or "" when it cannot be resolved -
// no token, gateway unreachable, or a channel deleted since the call was made.
// Callers fall back to the upstream host.
func (c *channelResolver) name(id int) string {
	if !c.enabled() {
		return ""
	}
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.names[id]
}

// refresh reloads the id->name map when it has expired.
//
// A failed load is not retried immediately. New API being down would otherwise
// cost every row of every list request a connection timeout, turning a missing
// label into an unusable page.
func (c *channelResolver) refresh() {
	if !c.stale() {
		return
	}
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	// Another request may have refreshed while this one waited for the lock.
	if !c.stale() {
		return
	}

	names, err := c.fetch()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.tried = time.Now()
	if err == nil {
		c.names, c.fetched = names, time.Now()
	}
}

func (c *channelResolver) stale() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	return now.Sub(c.fetched) >= c.ttl && now.Sub(c.tried) >= channelRetryEvery
}

type channelRow struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
}

func (c *channelResolver) fetch() (map[int]string, error) {
	out := map[int]string{}
	for p := 0; p < channelMaxPages; p++ {
		rows, err := c.page(p)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			// A name is optional in New API. Falling back to the address keeps
			// a nameless channel distinguishable from a channel we simply
			// failed to look up.
			if n := strings.TrimSpace(r.Name); n != "" {
				out[r.ID] = n
			} else if h := hostOf(r.BaseURL); h != "" {
				out[r.ID] = h
			}
		}
		if len(rows) < channelPageSize {
			break
		}
	}
	return out, nil
}

func (c *channelResolver) page(p int) ([]channelRow, error) {
	u := fmt.Sprintf("%s/api/channel/?p=%d&page_size=%d", c.base, p, channelPageSize)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("channel list: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseChannelPage(body)
}

// parseChannelPage accepts both shapes New API has used for this endpoint:
// {"data":{"items":[...]}} and {"data":[...]}. Guessing wrong costs only the
// channel name, so this tolerates the difference rather than pinning a version.
func parseChannelPage(body []byte) ([]channelRow, error) {
	var env struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("channel list rejected: %s", env.Message)
	}
	var paged struct {
		Items []channelRow `json:"items"`
	}
	if err := json.Unmarshal(env.Data, &paged); err == nil && paged.Items != nil {
		return paged.Items, nil
	}
	var flat []channelRow
	if err := json.Unmarshal(env.Data, &flat); err != nil {
		return nil, err
	}
	return flat, nil
}

// hostOf reduces an upstream URL to its host, which is all the list row has
// room for. The full URL stays on the record for the detail pane.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
