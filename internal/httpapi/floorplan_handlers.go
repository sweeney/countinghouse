package httpapi

import (
	"net/http"
	"sort"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
)

// floorEntry is one row in the /floors catalog: a floor id plus whatever the
// floorplan namespace declares about it.
type floorEntry struct {
	// ID is the value to pass back as floors= and the value devices carry in
	// their `floor` property.
	ID string `json:"id"`
	// Name is the floorplan's display label, empty when it declares none. The
	// catalog reports it empty rather than title-casing the id and hoping.
	Name string `json:"name"`
	// Order is the storey position, ascending from the lowest. Null when the
	// floorplan declares none — a pointer because 0 is a legitimate order (a
	// basement) and so cannot double as "undeclared". Without it every client
	// hardcodes its own storey sequence, since floor ids do not sort into
	// building order.
	Order *int `json:"order"`
	// Elevation is metres above the site datum, null when undeclared. Pointer for
	// the same reason as Order: 0.0 is a real elevation.
	Elevation *float64 `json:"elevation"`
	// DeviceCount is how many metered devices declare this floor. Always at least
	// 1, since a floor with none is not listed.
	DeviceCount int `json:"device_count"`
}

// handleFloors serves GET /floors: the floor catalog, and the discoverable
// vocabulary behind `floors=` and `group_by=floor`.
//
// WHICH floors are listed is the contract that matters. It is exactly the floors
// at least one BILLED device declares — metered, and not the whole-house meter —
// which is exactly the set `floors=` accepts and `group_by=floor` emits. All
// three come from energy.CountByGroupKey deliberately: if /floors advertised a
// floor that /series rejected with "unknown floor", a client filling a picker
// from this endpoint would build a broken control out of correct data. So a
// floorplan record naming a floor with no metered device is NOT listed — it
// exists in the building, but not as far as the energy API is concerned — and a
// floor devices declare but the floorplan has no record for IS listed, with its
// name empty and order null.
//
// That second case is why name and order are nullable rather than defaulted.
// Countinghouse passes the floorplan's answers through and reports UNKNOWN where
// it has none; it never derives a name from the id or a position from sort
// order, for the same reason it never derives a device's floor from its room id.
//
// Ordering: by declared order ascending, then by id, so floors with records come
// back in building order and undeclared ones sort last deterministically rather
// than wherever a map iteration left them.
func (s *Server) handleFloors(w http.ResponseWriter, _ *http.Request) {
	records := s.floors()
	counts := energy.CountByGroupKey(s.Config.Devices(), energy.GroupByFloor)

	out := make([]floorEntry, 0, len(counts))
	for id, n := range counts {
		e := floorEntry{ID: id, DeviceCount: n}
		if rec, ok := records[id]; ok {
			e.Name, e.Order, e.Elevation = rec.Name, rec.Order, rec.Elevation
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		return floorPrecedes(records, out[i].ID, out[j].ID)
	})

	writeJSON(w, http.StatusOK, map[string]any{"floors": out})
}

// roomEntry is one row in the /rooms catalog: a room id plus whatever the
// floorplan namespace declares about it.
type roomEntry struct {
	// ID is the value to pass back as rooms= and the value devices carry in
	// their `room` property. It is the key: NAMES ARE NOT UNIQUE, since two
	// rooms on different floors may share one.
	ID string `json:"id"`
	// Name is the floorplan's display label, empty when it declares none. The
	// catalog reports it empty rather than deriving one from the id.
	Name string `json:"name"`
	// Floor is the floor id the floorplan puts this room on, empty when it
	// declares none. Passed through from the room record — never read out of the
	// id's "<floor>.<slug>" shape, and never inferred from the floors this room's
	// devices declare.
	//
	// NOT guaranteed to appear in /floors or to be accepted by floors=. That
	// listing is built from the floors DEVICES declare; this relays what the ROOM
	// record declares, and countinghouse does not arbitrate between two upstream
	// declarations. They diverge whenever a device has a room but no declared
	// floor, which config.DeviceConfig.Floor explicitly allows. A client joining
	// /rooms to /floors must handle a miss.
	Floor string `json:"floor"`
	// Category is the room's purpose as the floorplan classifies it, e.g.
	// "kitchen", "circulation", "plant". Relayed RAW rather than reduced to a
	// flag, which matters most for energy: a plant room's consumption is
	// infrastructure rather than household usage, but which rooms "count" is a
	// per-dashboard policy question — a breakdown, a coverage view and a
	// heat-loss view answer it differently.
	Category string `json:"category"`
	// Area is the floor area in square metres, null when undeclared. A pointer
	// because absence differs from zero.
	Area *float64 `json:"area"`
	// DeviceCount is how many metered devices sit in this room. Always at least
	// 1, since a room with none is not listed.
	DeviceCount int `json:"device_count"`
}

// handleRooms serves GET /rooms: the room catalog, and the discoverable
// vocabulary behind `rooms=` — the room-shaped sibling of /floors.
//
// WHICH rooms are listed follows /floors exactly, and for the same reason: the
// rooms at least one BILLED device is attributed to, which is exactly the set
// `rooms=` accepts and `group_by=room` emits. A picker filled from this endpoint
// therefore cannot produce a 400. A floorplan record for a room holding no
// metered device is NOT listed, and a room devices declare that the floorplan
// has no record for IS listed, with its name, floor and category empty and area
// null.
//
// The reserved "house" key never appears. A device whose readings describe the
// whole property groups under it rather than under the room it sits in (see
// energy.GroupKeyFor), but that is a coverage scope, not a room: listing it
// would advertise a room id the taxonomy forbids.
//
// Ordering: by floor as the floorplan orders it (declared storey order, then
// floor id), then by room id — so a client renders the list in building order
// top to bottom without re-sorting, and rooms whose floor is unknown sort last
// together rather than being scattered.
func (s *Server) handleRooms(w http.ResponseWriter, _ *http.Request) {
	records := s.rooms()
	counts := energy.CountByGroupKey(s.Config.Devices(), energy.GroupByRoom)

	floors := s.floors()
	out := make([]roomEntry, 0, len(counts))
	for id, n := range counts {
		e := roomEntry{ID: id, DeviceCount: n}
		if rec, ok := records[id]; ok {
			e.Name, e.Floor, e.Category, e.Area = rec.Name, rec.Floor, rec.Category, rec.Area
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Floor != b.Floor {
			return floorPrecedes(floors, a.Floor, b.Floor)
		}
		return a.ID < b.ID
	})

	writeJSON(w, http.StatusOK, map[string]any{"rooms": out})
}

// floorPrecedes reports whether floor a sorts before floor b: declared storey
// order ascending, undeclared last, ties broken by id. Both catalogs sort
// through it, so they cannot order the same floors differently.
//
// A room whose floor is UNKNOWN ("") sorts after every known floor rather than
// first, so unplaced rooms gather at the end instead of leading the list.
//
// A floor named by a room record but absent from the floor records — possible,
// because /floors lists the floors DEVICES declare while a room relays what its
// own record declares (see roomEntry.Floor) — lands on the zero FloorConfig and
// so has no order. That is deliberate: such a floor is genuinely unordered as
// far as countinghouse knows, so it sorts with the other order-less floors
// rather than being given a position nobody published.
func floorPrecedes(floors map[string]config.FloorConfig, a, b string) bool {
	if a == "" || b == "" {
		return a != "" // a known floor precedes an unknown one
	}
	ao, bo := floors[a].Order, floors[b].Order
	switch {
	case ao != nil && bo != nil && *ao != *bo:
		return *ao < *bo
	case ao != nil && bo == nil:
		return true
	case ao == nil && bo != nil:
		return false
	}
	return a < b
}
