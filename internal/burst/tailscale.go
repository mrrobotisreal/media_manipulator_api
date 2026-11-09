package burst

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// KeyMinter mints a one-use Tailscale auth key for a burst instance.
type KeyMinter interface {
	Mint(ctx context.Context) (string, error)
}

// TailscaleMinter mints keys through the Tailscale API using an OAuth client
// (client-credentials). The key is a secret: it is returned to the caller and
// is never logged or stored. The OAuth token is cached until shortly before
// expiry.
//
// API (verified 2026-11-08): POST {base}/api/v2/oauth/token with form
// client_id/client_secret returns access_token, token_type Bearer,
// expires_in; POST {base}/api/v2/tailnet/{tailnet}/keys with a Bearer token
// and {capabilities.devices.create{reusable,ephemeral,preauthorized,tags},
// expirySeconds} returns {key,...}. The OAuth client needs the auth_keys scope
// with the tag.
type TailscaleMinter struct {
	BaseURL      string // default https://api.tailscale.com
	ClientID     string
	ClientSecret string
	Tailnet      string
	Tag          string
	HTTP         *http.Client
	Log          *slog.Logger
	Now          func() time.Time
	// KeyTTLSeconds is expirySeconds (default 600).
	KeyTTLSeconds int

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (m *TailscaleMinter) base() string {
	if m.BaseURL != "" {
		return strings.TrimRight(m.BaseURL, "/")
	}
	return "https://api.tailscale.com"
}

func (m *TailscaleMinter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *TailscaleMinter) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (m *TailscaleMinter) accessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token != "" && m.now().Add(60*time.Second).Before(m.expires) {
		return m.token, nil
	}
	form := url.Values{"client_id": {m.ClientID}, "client_secret": {m.ClientSecret}, "grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base()+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("tailscale token: %w", scrub(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tailscale token: status %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", errors.New("tailscale token: unreadable response")
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 3600
	}
	m.token = out.AccessToken
	m.expires = m.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return m.token, nil
}

// KeyRequest is the create-key body (exported for tests).
type KeyRequest struct {
	Capabilities  KeyCapabilities `json:"capabilities"`
	ExpirySeconds int             `json:"expirySeconds"`
}

type KeyCapabilities struct {
	Devices struct {
		Create KeyCreate `json:"create"`
	} `json:"devices"`
}

type KeyCreate struct {
	Reusable      bool     `json:"reusable"`
	Ephemeral     bool     `json:"ephemeral"`
	Preauthorized bool     `json:"preauthorized"`
	Tags          []string `json:"tags"`
}

// Mint returns a single-use, ephemeral, preauthorized, tagged key that expires
// in 10 minutes. Errors never contain the key or the client secret.
func (m *TailscaleMinter) Mint(ctx context.Context) (string, error) {
	tok, err := m.accessToken(ctx)
	if err != nil {
		return "", err
	}
	ttl := m.KeyTTLSeconds
	if ttl <= 0 {
		ttl = 600
	}
	var kr KeyRequest
	kr.Capabilities.Devices.Create = KeyCreate{Reusable: false, Ephemeral: true, Preauthorized: true, Tags: []string{m.Tag}}
	kr.ExpirySeconds = ttl
	b, _ := json.Marshal(kr)
	u := m.base() + "/api/v2/tailnet/" + url.PathEscape(m.Tailnet) + "/keys"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("tailscale key: %w", scrub(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusUnauthorized {
		m.mu.Lock()
		m.token = "" // force a fresh token next time
		m.mu.Unlock()
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("tailscale key: status %d", resp.StatusCode)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Key == "" {
		return "", errors.New("tailscale key: unreadable response")
	}
	if m.Log != nil {
		m.Log.Info("burst: tailscale key minted", "ttlSeconds", ttl, "tag", m.Tag) // never the key
	}
	return out.Key, nil
}

// scrub drops the URL (which could carry nothing secret today, but keeps the
// error text free of anything but the cause).
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Device is the slice of a Tailscale device the worker lookup needs.
type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Hostname  string   `json:"hostname"`
	Addresses []string `json:"addresses"`
}

// ErrDeviceNotFound: no device with that hostname (yet).
var ErrDeviceNotFound = errors.New("tailscale: device not found")

// WorkerHostname is the tailnet hostname the user-data gives a run's instance.
func WorkerHostname(runID string) string {
	id := strings.ReplaceAll(runID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return "mm-burst-" + id
}

// DevicesURL is the list-devices URL. Tailnet "-" (the OAuth client's own
// tailnet) passes through PathEscape unchanged.
func (m *TailscaleMinter) DevicesURL() string {
	return m.base() + "/api/v2/tailnet/" + url.PathEscape(m.Tailnet) + "/devices"
}

// ResolveIP returns the tailnet IPv4 of the device named hostname (decision 7:
// the Tailscale devices API, scope Devices Core read). ErrDeviceNotFound until
// the instance has joined.
func (m *TailscaleMinter) ResolveIP(ctx context.Context, hostname string) (string, error) {
	tok, err := m.accessToken(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.DevicesURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := m.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("tailscale devices: %w", scrub(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		m.mu.Lock()
		m.token = ""
		m.mu.Unlock()
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tailscale devices: status %d", resp.StatusCode)
	}
	var out struct {
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", errors.New("tailscale devices: unreadable response")
	}
	for _, d := range out.Devices {
		short := d.Name
		if i := strings.IndexByte(short, '.'); i >= 0 {
			short = short[:i]
		}
		if d.Hostname != hostname && short != hostname {
			continue
		}
		for _, a := range d.Addresses {
			if !strings.Contains(a, ":") {
				return a, nil
			}
		}
	}
	return "", ErrDeviceNotFound
}

// Resolver finds a run's worker address (the minter implements it).
type Resolver interface {
	ResolveIP(ctx context.Context, hostname string) (string, error)
}
