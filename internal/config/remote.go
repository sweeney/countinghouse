package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TokenSource is satisfied by identity.TokenSource. The Fetcher uses it to
// obtain a Bearer token for the config service and to invalidate that token on
// a 401 so the next fetch retries with a fresh credential.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	// Invalidate clears any cached token. Called by Fetcher when the config
	// service responds with 401, so the next Token() call fetches a fresh one
	// rather than replaying the rejected credential for its full TTL.
	Invalidate()
}

// NamespaceStatus records the outcome of the most recent fetch attempt for a
// single config namespace, surfaced on /healthz.
type NamespaceStatus struct {
	OK        bool      `json:"ok"`
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
}

// Fetcher fetches the two remote config namespaces countinghouse depends on
// (statehouse_devices and energy_tariffs) and HOLDS them as live snapshots.
//
// Unlike statehouse — which merges remote config into a local Config struct —
// countinghouse is read-side and stateless: the Fetcher is the authoritative
// in-memory view that the HTTP handlers query via the ConfigProvider interface
// (Devices()/Tariffs()). main.go refreshes it once at startup and again on
// SIGHUP.
//
// Refresh is FAIL-OPEN: any error (token, transport, non-200, decode) is logged
// as a warning and the last-known-good snapshot for that namespace is kept. A
// failure on one namespace never wipes the other. This keeps the service
// serving the last good config across transient config-service outages, which
// matches the stateless invariant (truth is rebuildable; never lose good data).
type Fetcher struct {
	BaseURL    string
	Tokens     TokenSource
	HTTPClient *http.Client
	Logger     *slog.Logger

	mu       sync.RWMutex
	devices  map[string]DeviceConfig
	tariffs  EnergyTariffs
	statuses map[string]NamespaceStatus
}

// defaultFetchClient is used when Fetcher.HTTPClient is nil.
var defaultFetchClient = &http.Client{Timeout: 10 * time.Second}

// maxConfigBytes is the maximum response body size accepted from the config service.
const maxConfigBytes = 1 << 20 // 1 MiB

// Namespace names fetched from the config service.
const (
	nsDevices = "statehouse_devices"
	nsTariffs = "energy_tariffs"
)

// Devices returns a copy of the current statehouse_devices snapshot keyed by
// device_id. Safe for concurrent use. Implements httpapi.ConfigProvider.
func (f *Fetcher) Devices() map[string]DeviceConfig {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]DeviceConfig, len(f.devices))
	for k, v := range f.devices {
		out[k] = v
	}
	return out
}

// Tariffs returns the current energy_tariffs snapshot. Safe for concurrent use.
// Implements httpapi.ConfigProvider. The returned EnergyTariffs shares the
// inner Tariffs map; callers treat it as read-only (handlers never mutate it).
func (f *Fetcher) Tariffs() EnergyTariffs {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.tariffs
}

// Statuses returns a snapshot of the last fetch result for each namespace.
// Implements httpapi.ConfigStatus.
func (f *Fetcher) Statuses() map[string]NamespaceStatus {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]NamespaceStatus, len(f.statuses))
	for k, v := range f.statuses {
		out[k] = v
	}
	return out
}

// Refresh fetches both namespaces and swaps the held snapshots on success.
// Fail-open: on any error the previous snapshot for that namespace is kept and
// a per-namespace status is recorded. Refresh never panics.
//
// (Named Refresh; ApplyRemote is the statehouse spelling — here the verb is a
// refresh of held snapshots, not a merge into a Config.)
func (f *Fetcher) Refresh(ctx context.Context) {
	if !strings.HasPrefix(f.BaseURL, "https://") {
		f.warn("remote config: base_url is not https — bearer token will be transmitted in cleartext", "url", f.BaseURL)
	}
	if f.BaseURL == "" {
		// No remote config configured; serve empty snapshots. Don't attempt a
		// token fetch or record a status — there is nothing to fetch.
		return
	}
	token, err := f.Tokens.Token(ctx)
	if err != nil {
		f.warn("remote config: identity token fetch failed, keeping last-known snapshots", "error", err)
		f.recordStatus(nsDevices, err)
		f.recordStatus(nsTariffs, err)
		return
	}
	f.refreshDevices(ctx, token)
	f.refreshTariffs(ctx, token)
}

func (f *Fetcher) refreshDevices(ctx context.Context, token string) {
	var devices map[string]DeviceConfig
	if err := f.fetch(ctx, token, nsDevices, &devices); err != nil {
		f.warn("remote config: statehouse_devices unavailable, keeping last-known", "error", err)
		f.recordStatus(nsDevices, err)
		return
	}
	if devices == nil {
		devices = map[string]DeviceConfig{}
	}
	normaliseDevices(devices)
	f.mu.Lock()
	f.devices = devices
	f.mu.Unlock()
	f.recordStatus(nsDevices, nil)
}

func (f *Fetcher) refreshTariffs(ctx context.Context, token string) {
	var tariffs EnergyTariffs
	if err := f.fetch(ctx, token, nsTariffs, &tariffs); err != nil {
		f.warn("remote config: energy_tariffs unavailable, keeping last-known", "error", err)
		f.recordStatus(nsTariffs, err)
		return
	}
	f.mu.Lock()
	f.tariffs = tariffs
	f.mu.Unlock()
	f.recordStatus(nsTariffs, nil)
}

func (f *Fetcher) recordStatus(ns string, err error) {
	s := NamespaceStatus{OK: err == nil, FetchedAt: time.Now()}
	if err != nil {
		s.Error = err.Error()
	}
	f.mu.Lock()
	if f.statuses == nil {
		f.statuses = make(map[string]NamespaceStatus)
	}
	f.statuses[ns] = s
	f.mu.Unlock()
}

func (f *Fetcher) fetch(ctx context.Context, token, ns string, dst any) error {
	client := f.HTTPClient
	if client == nil {
		client = defaultFetchClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.BaseURL+"/api/v1/config/"+ns, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		f.Tokens.Invalidate()
		return fmt.Errorf("unauthorized: token may be stale, invalidated for next retry")
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigBytes+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxConfigBytes {
		return fmt.Errorf("config response exceeds %d bytes", maxConfigBytes)
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	return nil
}

func (f *Fetcher) warn(msg string, args ...any) {
	if f.Logger != nil {
		f.Logger.Warn(msg, args...)
	}
}
