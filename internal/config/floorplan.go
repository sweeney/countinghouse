package config

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// FloorConfig is one floor record as the floorplan namespace publishes it.
//
// Countinghouse passes these through unchanged, exactly as it does the `floor` a
// device declares. The floorplan owns the taxonomy: a client that had to sort
// floor ids into building order, or title-case one into a label, would be a
// second implementation of someone else's data — living in every consumer
// instead of in one service, and wrong the moment it met a different building.
// The same records back greenhouse's /floors, so both services answer a page
// asking about the same house in the same vocabulary (issue #19).
//
// Every field is optional. A namespace that publishes only names still improves
// on nothing, and a record that omits Order leaves ordering UNKNOWN rather than
// asserting a position countinghouse invented.
type FloorConfig struct {
	// ID is the floor id devices reference in their `floor` property and callers
	// pass to `floors=`. It is normally the map key in the namespace document;
	// normaliseFloors fills it from the key when a record omits it, and the KEY
	// always wins so the two can never disagree.
	ID string `yaml:"id" json:"id,omitempty"`

	// Name is the human-readable label, e.g. "Floor One". Empty when the
	// namespace declares none: the catalog reports it empty rather than
	// title-casing the id and hoping.
	Name string `yaml:"name" json:"name,omitempty"`

	// Order is the position in the building, ascending from the lowest storey.
	// It is a POINTER because absence is meaningful and distinct from zero: a
	// basement legitimately sits at order 0, so a plain int could not tell
	// "ground level" from "undeclared". Nil means UNKNOWN, which the catalog
	// reports as null and sorts last.
	Order *int `yaml:"order" json:"order,omitempty"`

	// Elevation is the floor's height in metres above the site datum, published
	// alongside order. Passed through when declared; nil is UNKNOWN, and it is a
	// pointer for the same reason as Order — 0.0 is a real elevation.
	Elevation *float64 `yaml:"elevation" json:"elevation,omitempty"`
}

// RoomConfig is one room record as the floorplan namespace publishes it.
//
// Passed through unchanged for the same reason as FloorConfig: the floorplan
// owns the taxonomy, countinghouse relays it, clients interpret it.
type RoomConfig struct {
	// ID is the floorplan room id devices reference in their `room` property and
	// callers pass to `rooms=`, e.g. "floor2.room-a". It is the map key in the
	// namespace document; normaliseRooms fills it from the key when a record
	// omits it, and the KEY always wins so the two can never disagree.
	ID string `yaml:"id" json:"id,omitempty"`

	// Name is the human-readable label, e.g. "Room A". Empty when the namespace
	// declares none: the catalog reports it empty rather than deriving one from
	// the id.
	//
	// Names are NOT unique — two rooms on different floors may share one. The id
	// is the key, and a client wanting an unambiguous label composes the floor's
	// name with this one.
	Name string `yaml:"name" json:"name,omitempty"`

	// Floor is the floor id this room sits on. Passed through from the room
	// record, never read out of the id's "<floor>.<slug>" shape and never
	// inferred from the floors its devices declare: those are separate
	// declarations that can disagree, and countinghouse does not arbitrate
	// between two upstream answers. Empty means the floorplan declared none.
	Floor string `yaml:"floor" json:"floor,omitempty"`

	// Category is the room's purpose as the floorplan classifies it, e.g.
	// "kitchen", "circulation", "plant". Relayed RAW and never reduced to a
	// boolean, which matters especially for energy: a plant room's consumption is
	// infrastructure rather than household usage, but WHICH rooms "count" is a
	// per-dashboard policy question. A breakdown, a coverage view and a heat-loss
	// view answer it differently, so a computed flag would bake the first
	// caller's answer into the API and leave the other two working around it.
	Category string `yaml:"category" json:"category,omitempty"`

	// Area is the room's floor area in square metres. A POINTER because absence
	// differs from zero, the same reason FloorConfig.Order and Elevation are:
	// nil is UNKNOWN and the catalog reports it null.
	Area *float64 `yaml:"area" json:"area,omitempty"`
}

// normaliseFloors fills each record's ID from its map key.
//
// The key is AUTHORITATIVE: it is what a device's `floor` property references
// and what `floors=` matches, so a record whose inner id disagrees with its key
// would otherwise publish an id nothing can be filtered by. Mirrors
// normaliseDevices, which treats the device_id key the same way.
func normaliseFloors(floors map[string]FloorConfig) {
	for id, f := range floors {
		f.ID = id
		floors[id] = f
	}
}

// normaliseRooms fills each record's ID from its map key, for the same reason
// normaliseFloors does.
func normaliseRooms(rooms map[string]RoomConfig) {
	for id, r := range rooms {
		r.ID = id
		rooms[id] = r
	}
}

// floorplanDocument is the decoded floorplan namespace document: the building's
// floors AND its rooms, each keyed by id, whichever of two shapes the namespace
// publishes.
//
// The published shape wraps ARRAYS, each record carrying its own id:
//
//	{"floors": [{"id": "floor1", "name": "Floor One", "order": 1}, ...],
//	 "rooms":  [{"id": "floor1.room-a", "name": "Room A", "floor": "floor1",
//	             "category": "utility", "area": 12.4}, ...]}
//
// The devices namespace, by contrast, is a MAP keyed by id, and greenhouse
// originally assumed the floorplan matched it:
//
//	{"floor1": {"name": "Floor One", "order": 1}, ...}
//
// Both are accepted, mirroring greenhouse exactly — the two services read the
// SAME document, so a shape one accepts and the other rejects would mean two
// answers about one house. Being liberal here is not indecision about the
// format: the namespace is owned by another service, countinghouse cannot deploy
// in lockstep with it, and the failure mode of guessing wrong is fail-open
// silence — blank names and null order, indistinguishable from a floorplan
// namespace nobody configured.
//
// Liberal in what it ACCEPTS, never in what it drops. A document mixing the two
// shapes — one collection as an array, the other as an object — is refused rather
// than half-read, because dropping the odd collection silently produces exactly
// the "publishes nothing" appearance this service refuses everywhere else. See
// UnmarshalJSON.
//
// The shapes are told apart by JSON type, not by key name: a "floors" key
// holding an object is a floor whose id happens to be "floors", and decodes as
// the map shape — PROVIDED no sibling "rooms" array is present. The case needs a
// floor literally named "floors" alongside a rooms array, which has never
// existed; it is documented rather than handled so the rule is not read as
// broader than it is. The map shape carries FLOORS ONLY — it predates rooms
// being published and there is no room-shaped reading of it, so Rooms is empty
// there and every room's name and category are honestly UNKNOWN rather than
// guessed. Unmodelled keys (e.g. "ceiling") are ignored throughout.
type floorplanDocument struct {
	Floors map[string]FloorConfig
	Rooms  map[string]RoomConfig
}

func (d *floorplanDocument) UnmarshalJSON(b []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	// A null body unmarshals into a nil map without error, and would otherwise
	// fall through to the map branch and decode as a document with no records —
	// latching a SUCCESSFUL fetch (see Fetcher.Cold) for a namespace that
	// published nothing readable. `{}` is different and stays valid: it is a
	// namespace that published no records, which is an answer.
	if probe == nil {
		return fmt.Errorf("floorplan document is null, not an object")
	}

	floorsRaw, hasFloors := probe["floors"]
	roomsRaw, hasRooms := probe["rooms"]

	// The wrapper shape is identified by either collection arriving as an ARRAY.
	// Checking both means a document that publishes rooms but no floors (or vice
	// versa) is still read as the wrapper it is, rather than falling through to
	// the map branch and failing to unmarshal an array into a FloorConfig.
	if (hasFloors && isJSONArray(floorsRaw)) || (hasRooms && isJSONArray(roomsRaw)) {
		// In the wrapper shape BOTH collections are arrays. One published as an
		// object (or anything else) is refused rather than skipped: skipping drops
		// every record in it with no error and no warning, which is
		// indistinguishable from a floorplan publishing none — the same silence
		// the required-namespace and cold-start refusals exist to remove, one layer
		// further in. Refused, the fetch fails, recordStatus records it, /healthz
		// degrades, and at boot Cold() refuses to start.
		//
		// Loud rather than LIBERAL: greenhouse's decoder has the same asymmetry, so
		// accepting an object here would mean the two services read different rooms
		// out of one document. This keeps the accepted set identical and only
		// changes what happens to a document neither can read properly.
		if hasFloors && !isJSONArray(floorsRaw) {
			return fmt.Errorf("floorplan document publishes \"rooms\" as an array but " +
				"\"floors\" as something else: in the wrapper shape both are arrays of " +
				"records, and skipping the odd one out would drop every floor silently")
		}
		if hasRooms && !isJSONArray(roomsRaw) {
			return fmt.Errorf("floorplan document publishes \"floors\" as an array but " +
				"\"rooms\" as something else: in the wrapper shape both are arrays of " +
				"records, and skipping the odd one out would drop every room silently")
		}
		out := floorplanDocument{
			Floors: map[string]FloorConfig{},
			Rooms:  map[string]RoomConfig{},
		}
		if hasFloors {
			var list []FloorConfig
			if err := json.Unmarshal(floorsRaw, &list); err != nil {
				return err
			}
			for _, f := range list {
				// A record with no id cannot be referenced by a device's `floor`
				// property or matched by floors=, so it is unusable rather than
				// merely unlabelled. Skipped instead of keyed on "", which would
				// invent a floor nothing can select. A later duplicate wins, as it
				// would in a JSON object.
				if f.ID == "" {
					continue
				}
				out.Floors[f.ID] = f
			}
		}
		if hasRooms {
			var list []RoomConfig
			if err := json.Unmarshal(roomsRaw, &list); err != nil {
				return err
			}
			for _, r := range list {
				// Same rule, same reason: an id-less room is unreferenceable by a
				// device's `room` property and unmatchable by rooms=.
				if r.ID == "" {
					continue
				}
				out.Rooms[r.ID] = r
			}
		}
		*d = out
		return nil
	}

	var keyed map[string]FloorConfig
	if err := json.Unmarshal(b, &keyed); err != nil {
		return err
	}
	*d = floorplanDocument{Floors: keyed}
	return nil
}

// isJSONArray reports whether raw is a JSON array, ignoring leading whitespace.
func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}
