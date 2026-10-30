package config

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/sweeney/common/remoteconfig"
)

// TokenSource is satisfied by identity.TokenSource. remoteconfig uses it to
// obtain a Bearer token for the config service, and invalidates that token on a
// 401 before retrying once with a fresh credential.
type TokenSource = remoteconfig.TokenSource

// NamespaceStatus records the outcome of the most recent fetch attempt for a
// single config namespace, surfaced on /healthz.
type NamespaceStatus = remoteconfig.Status

// Fetcher fetches the three remote config namespaces countinghouse depends on —
// this site's devices namespace, energy_tariffs, and this site's floorplan
// namespace — and HOLDS them as live snapshots.
//
// Unlike statehouse — which merges remote config into a local Config struct —
// countinghouse is read-side and stateless: the Fetcher is the authoritative
// in-memory view that the HTTP handlers query via the ConfigProvider interface
// (Devices()/Tariffs()/Floors()/Rooms()). main.go refreshes it once at startup
// and again on SIGHUP.
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
	// UserAgent is sent to the config service, conventionally
	// "countinghouse/<version>". Empty sends remoteconfig's default.
	UserAgent string

	// DevicesNamespace names the namespace holding this site's devices. Empty means
	// the shared pre-migration document, so a Fetcher built without it behaves
	// exactly as it did before devices were split per site.
	DevicesNamespace string

	// FloorplanNamespace names the namespace holding this site's floor and room
	// records, published by /floors and /rooms and used to label grouped series.
	// Load requires it, so an empty value here means a Fetcher built by hand; the
	// fetch is then skipped with a warning rather than requesting /api/v1/config/,
	// exactly as the devices fetch handles the same mistake.
	FloorplanNamespace string

	// EnergyAgreementsNamespace names the dated-tariff namespace. When empty the legacy
	// energy_tariffs document is authoritative; when set, IT is, and the legacy
	// document is not fetched at all. Never merged — see RemoteConfigConfig.
	EnergyAgreementsNamespace string

	// resolved records the namespaces settled at startup, and the site they were
	// settled for, so a later refresh can report DRIFT without adopting it.
	resolved   SiteNamespaces
	resolvedID string

	mu         sync.RWMutex
	devices    map[string]DeviceConfig
	floors     map[string]FloorConfig
	rooms      map[string]RoomConfig
	tariffs    EnergyTariffs
	agreements EnergyAgreements

	// tracker holds each namespace's latest status and whether it has ever
	// landed, which is what separates STALE from COLD. Landing is not derivable
	// from the snapshots: an empty document is a successful fetch of "nothing
	// here", which is an answer rather than an absence. A success latches for the
	// life of the process, so a transient config-service blip can never promote
	// a healthy instance into a refusal.
	tracker remoteconfig.Tracker
}

// nsTariffs is the one namespace that is still the same document for every site.
// The devices namespace is per-site and comes from config — see DevicesNamespace.
const nsTariffs = "energy_tariffs"

// nsSites is the shared per-site document. It is BOOT-CRITICAL in a way the others
// are not: it names which namespaces to read, so without it there is nothing to
// fail open onto — we could not even say what we are missing.
const nsSites = "sites"

// ResolveNamespaces fetches the shared `sites` document and settles which
// namespaces this instance will read, writing them onto the Fetcher.
//
// This is phase one of a two-phase bootstrap: until `sites` has been read we
// cannot name the other namespaces, so there is no meaningful fail-open here —
// falling open would mean reading some other property's data, or none. A failure
// is therefore an error for the caller to abort on, consistent with the
// cold-start rule everywhere else.
//
// Local config is the fallback and `sites` wins a disagreement; see
// ResolveSiteNamespaces for why that direction, and for the warnings returned.
//
// With no BaseURL configured nothing is fetched and local config stands alone,
// which is what keeps local development working.
func (f *Fetcher) ResolveNamespaces(ctx context.Context, local SiteConfig) ([]string, error) {
	var sites Sites

	if f.BaseURL != "" {
		if err := f.client().Get(ctx, nsSites, &sites); err != nil {
			f.tracker.Record(nsSites, err)
			return nil, fmt.Errorf("config: cannot resolve site namespaces: fetching %q: %w", nsSites, err)
		}
		f.tracker.Record(nsSites, nil)
	}

	resolved, warns, err := ResolveSiteNamespaces(local, sites)
	if err != nil {
		return warns, err
	}

	f.DevicesNamespace = resolved.Devices
	f.FloorplanNamespace = resolved.Floorplan
	f.EnergyAgreementsNamespace = resolved.Agreements

	f.mu.Lock()
	f.resolved, f.resolvedID = resolved, local.ID
	f.mu.Unlock()

	return warns, nil
}

// SiteNamespaceDrift re-reads `sites` and reports pointers that have changed since
// startup, WITHOUT adopting them.
//
// Repointing a running service at a different property's data is not something to
// do silently: the device inventory would swap underneath every in-flight answer.
// So a change is reported for an explicit restart, which is cheap. Returns nothing
// when there is no remote config to compare against.
func (f *Fetcher) SiteNamespaceDrift(ctx context.Context) []string {
	if f.BaseURL == "" {
		return nil
	}
	var sites Sites
	if err := f.client().Get(ctx, nsSites, &sites); err != nil {
		return nil
	}
	f.mu.RLock()
	resolved, id := f.resolved, f.resolvedID
	f.mu.RUnlock()
	if id == "" {
		return nil
	}
	return resolved.DriftFrom(sites, id)
}

// Devices returns a copy of the current devices snapshot keyed by
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

// Tariffs returns whichever tariff document is authoritative, as a TariffSource.
// Safe for concurrent use. Implements httpapi.ConfigProvider.
//
// Returning the INTERFACE rather than a concrete document is what keeps the
// migration out of the HTTP layer: every caller asks TariffFor(t) or
// PeriodsBetween and neither knows nor cares which namespace answered. The
// snapshots share their inner maps and slices; callers treat them as read-only.
func (f *Fetcher) Tariffs() TariffSource {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.EnergyAgreementsNamespace != "" {
		return f.agreements
	}
	return f.tariffs
}

// Agreements returns the authoritative document in the dated-block shape, so
// /tariffs serves ONE response shape whichever namespace backs it rather than
// making every consumer handle both. A legacy document is presented as a single
// open-ended fixed agreement.
func (f *Fetcher) Agreements() EnergyAgreements {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.EnergyAgreementsNamespace != "" {
		return f.agreements
	}
	return f.tariffs.AsAgreements()
}

// TariffNamespace reports which namespace is authoritative, for /healthz and
// /tariffs. Which document priced a bill is not something anybody should have to
// deduce from the shape of the answer.
func (f *Fetcher) TariffNamespace() string {
	if f.EnergyAgreementsNamespace != "" {
		return f.EnergyAgreementsNamespace
	}
	return nsTariffs
}

// Floors returns a copy of the current floor-record snapshot keyed by floor id.
// Safe for concurrent use. Implements httpapi.FloorplanProvider.
//
// An empty map means either that no floorplan namespace is configured or that
// none has been fetched yet. Both are UNKNOWN rather than "there are no floors":
// callers enrich what they have and report the rest empty, never inferring that
// a floor does not exist because its record is missing.
func (f *Fetcher) Floors() map[string]FloorConfig {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]FloorConfig, len(f.floors))
	for k, v := range f.floors {
		out[k] = v
	}
	return out
}

// Rooms returns a copy of the current room-record snapshot keyed by floorplan
// room id. Safe for concurrent use. Implements httpapi.FloorplanProvider.
//
// An empty map means no floorplan namespace is configured, none has been fetched
// yet, or the document is the legacy floors-only map shape. All three are
// UNKNOWN rather than "there are no rooms".
func (f *Fetcher) Rooms() map[string]RoomConfig {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make(map[string]RoomConfig, len(f.rooms))
	for k, v := range f.rooms {
		out[k] = v
	}
	return out
}

// Cold returns the namespaces this Fetcher is configured to read that have NEVER
// been fetched successfully, sorted for a deterministic message.
//
// Cold is the state fail-open cannot cover. Every other failure keeps a
// last-known snapshot, so the service carries on serving the truth it had; with
// nothing ever fetched it falls open onto emptiness instead — no devices, no
// tariffs, no floor names — and every endpoint then answers zero, or ids where
// names belong, in the confident shape of a correct response. That is the same
// silence requireDevicesNamespace and requireFloorplanNamespace refuse at config
// level, arriving one layer later.
//
// So main.go refuses to boot on a non-empty result: boot needs truth, running
// keeps the last truth. It is deliberately a QUERY rather than an error returned
// by Refresh, because the two callers want opposite things — startup must refuse,
// and the SIGHUP reload must not, since by then a snapshot exists.
//
// A Fetcher with no BaseURL attempts nothing and so reports every configured
// namespace; main.go treats an empty base_url as an explicit local-dev opt-out and
// only consults Cold when a config service is named.
func (f *Fetcher) Cold() []string {
	return f.tracker.Cold(f.configuredNamespaces()...)
}

// configuredNamespaces lists the namespaces this Fetcher reads. An unnamed one is
// skipped rather than reported: Load requires both site namespaces, so an empty
// name means a Fetcher built by hand, and reporting "" as cold would name nothing
// an operator could act on.
func (f *Fetcher) configuredNamespaces() []string {
	out := []string{}
	if f.EnergyAgreementsNamespace != "" {
		out = append(out, f.EnergyAgreementsNamespace)
	} else {
		out = append(out, nsTariffs)
	}
	if ns := f.devicesNamespace(); ns != "" {
		out = append(out, ns)
	}
	if f.FloorplanNamespace != "" {
		out = append(out, f.FloorplanNamespace)
	}
	// sites is included once it has been resolved from: it is how the others were
	// named, so a cold sites namespace means we never knew what to read.
	f.mu.RLock()
	resolvedID := f.resolvedID
	f.mu.RUnlock()
	if resolvedID != "" && f.BaseURL != "" {
		out = append(out, nsSites)
	}
	return out
}

// Statuses returns a snapshot of the last fetch result for each namespace.
// Implements httpapi.ConfigStatus.
func (f *Fetcher) Statuses() map[string]NamespaceStatus {
	return f.tracker.Statuses()
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
	if f.Tokens != nil {
		// One token serves every namespace: remoteconfig asks for it per request,
		// and the TokenSource answers from its cache. Asking here first means a
		// token that cannot be had costs identity one call rather than one per
		// namespace, and that every namespace records the real cause. Otherwise a
		// hung identity would spend the whole refresh deadline on devices, and the
		// rest would record only the deadline.
		if _, err := f.Tokens.Token(ctx); err != nil {
			err = fmt.Errorf("remote config token: %w", err)
			for _, ns := range []string{f.devicesNamespace(), f.TariffNamespace(), f.FloorplanNamespace} {
				if ns != "" {
					f.warn("remote config: namespace unavailable, keeping last-known",
						"namespace", ns, "error", err)
					f.tracker.Record(ns, err)
				}
			}
			return
		}
	}
	// Built once per Refresh, so one refresh reads every namespace with the same
	// settings.
	c := f.client()
	f.refreshDevices(ctx, c)
	if f.EnergyAgreementsNamespace != "" {
		f.refreshAgreements(ctx, c)
	} else {
		f.refreshTariffs(ctx, c)
	}
	f.refreshFloorplan(ctx, c)
}

// refreshFloorplan fetches the floorplan namespace, which carries both the
// building's floors and its rooms.
//
// Fail-open like the other two: a failing fetch keeps the last-known records and
// records a status, so an operator can see the names are not arriving, while the
// endpoints that bill carry on unaffected. That resilience is why the NAME is
// required at load time and the DOCUMENT is not: countinghouse insists an
// operator says where the records live, then tolerates the config service being
// down.
func (f *Fetcher) refreshFloorplan(ctx context.Context, c *remoteconfig.Client) {
	if f.FloorplanNamespace == "" {
		// Load refuses a config naming no floorplan namespace, so reaching here
		// means a Fetcher built by hand. Concatenating an empty name would request
		// /api/v1/config/ — a different endpoint, failing for a reason that says
		// nothing about the actual mistake — so say so instead of guessing.
		f.warn("remote config: no floorplan namespace configured, skipping the floorplan " +
			"fetch (a Fetcher built without going through config.Load)")
		return
	}
	var doc floorplanDocument
	if err := c.Get(ctx, f.FloorplanNamespace, &doc); err != nil {
		f.warn("remote config: floorplan namespace unavailable, keeping last-known",
			"namespace", f.FloorplanNamespace, "error", err)
		f.tracker.Record(f.FloorplanNamespace, err)
		return
	}
	floors, rooms := doc.Floors, doc.Rooms
	if floors == nil {
		floors = map[string]FloorConfig{}
	}
	if rooms == nil {
		rooms = map[string]RoomConfig{}
	}
	normaliseFloors(floors)
	normaliseRooms(rooms)
	// Swapped together: they come from one document, so a reader must never see
	// this fetch's rooms beside the previous fetch's floors.
	f.mu.Lock()
	f.floors, f.rooms = floors, rooms
	f.mu.Unlock()
	f.tracker.Record(f.FloorplanNamespace, nil)
}

func (f *Fetcher) refreshDevices(ctx context.Context, c *remoteconfig.Client) {
	ns := f.devicesNamespace()
	// The request path is built by concatenation, so an unnamed namespace would ask for
	// /api/v1/config/ — a different endpoint, failing for a reason that says nothing
	// about the actual mistake. That trades a silent failure for a confusing one, so
	// refuse instead. Load rejects a config that names no namespace, so reaching here
	// means a Fetcher built by hand; say so rather than guessing a name.
	if ns == "" {
		f.warn("remote config: no devices namespace configured, skipping the devices fetch " +
			"(a Fetcher built without going through config.Load)")
		return
	}

	// No nil guard on the decoded map: remoteconfig refuses any body that is not a
	// JSON object, and decoding an object into a nil map always allocates it.
	var devices map[string]DeviceConfig
	if err := c.Get(ctx, ns, &devices); err != nil {
		f.warn("remote config: devices namespace unavailable, keeping last-known",
			"namespace", ns, "error", err)
		f.tracker.Record(ns, err)
		return
	}
	normaliseDevices(devices)
	f.mu.Lock()
	f.devices = devices
	f.mu.Unlock()
	f.tracker.Record(ns, nil)
}

func (f *Fetcher) refreshTariffs(ctx context.Context, c *remoteconfig.Client) {
	var tariffs EnergyTariffs
	if err := c.Get(ctx, nsTariffs, &tariffs); err != nil {
		f.warn("remote config: energy_tariffs unavailable, keeping last-known", "error", err)
		f.tracker.Record(nsTariffs, err)
		return
	}
	// A document that fetched fine but is internally ambiguous is treated
	// exactly like a fetch failure: keep the last-known snapshot and record the
	// reason. Applying it would let an ambiguous tariff price money, and
	// tariffs are the one namespace where a plausible-looking wrong number is
	// worse than no update at all.
	//
	// Combined with the cold-start rule (Cold() aborts startup for a namespace
	// that has NEVER been fetched), this gives the project's standard shape:
	// boot needs truth, running keeps the last truth.
	if err := tariffs.Validate(); err != nil {
		f.warn("remote config: energy_tariffs is invalid, keeping last-known", "error", err)
		f.tracker.Record(nsTariffs, err)
		return
	}
	f.mu.Lock()
	f.tariffs = tariffs
	f.mu.Unlock()
	f.tracker.Record(nsTariffs, nil)
}

// refreshAgreements fetches the dated-tariff document.
//
// Same policy as refreshTariffs: a fetch failure OR an invalid document keeps the
// last-known snapshot and records the reason, because applying an ambiguous
// tariff document would let it price money. With the cold-start rule, boot needs
// truth and running keeps the last truth.
func (f *Fetcher) refreshAgreements(ctx context.Context, c *remoteconfig.Client) {
	ns := f.EnergyAgreementsNamespace
	var agreements EnergyAgreements
	if err := c.Get(ctx, ns, &agreements); err != nil {
		f.warn("remote config: agreements namespace unavailable, keeping last-known",
			"namespace", ns, "error", err)
		f.tracker.Record(ns, err)
		return
	}
	if err := agreements.Validate(); err != nil {
		f.warn("remote config: agreements namespace is invalid, keeping last-known",
			"namespace", ns, "error", err)
		f.tracker.Record(ns, err)
		return
	}
	f.mu.Lock()
	f.agreements = agreements
	f.mu.Unlock()
	f.tracker.Record(ns, nil)
}

// client builds the remoteconfig.Client for one entry point: a Refresh, a
// ResolveNamespaces or a SiteNamespaceDrift.
func (f *Fetcher) client() *remoteconfig.Client {
	return &remoteconfig.Client{
		BaseURL:    f.BaseURL,
		Tokens:     f.Tokens,
		UserAgent:  f.UserAgent,
		HTTPClient: f.HTTPClient,
	}
}

func (f *Fetcher) warn(msg string, args ...any) {
	if f.Logger != nil {
		f.Logger.Warn(msg, args...)
	}
}

// devicesNamespace is the namespace this fetcher reads devices from. There is no
// fallback: Load refuses a config that names none, and substituting one here would
// reintroduce the deleted shared namespace by the back door for any Fetcher built
// without going through Load. Defaulting the same value in two places is the shape of
// bug that recurred throughout this migration, so it now has exactly one source.
func (f *Fetcher) devicesNamespace() string {
	return f.DevicesNamespace
}
