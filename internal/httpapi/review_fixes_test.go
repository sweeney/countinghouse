package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Findings from the final review of efd503f.
// ---------------------------------------------------------------------------

// summary.current moves every half hour. On window=today the window bounds and
// the curve fingerprint are pinned for the WHOLE day, so a consumer polling with
// If-None-Match was told "unchanged" while the live price underneath it moved.
//
// The same class of bug the semantic tag exists to prevent, reintroduced by
// adding a field to the body without adding it to the tag.
func TestETagMovesWhenTheCurrentSlotDoes(t *testing.T) {
	midnight := londonMidnight(t, pxNow(t))
	rates := make([]float64, 48)
	for i := range rates {
		rates[i] = 20 + float64(i) // every slot a different price
	}
	slots := pxSlots(midnight, rates...)

	at := func(h, m int) string {
		s := pxSetup(t, slots)
		s.Clock = fixedClock{midnight.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)}
		w := doGET(t, s, "/prices?window=today")
		return w.Header().Get("ETag")
	}
	early, later := at(10, 0), at(13, 0)
	if early == "" || later == "" {
		t.Fatal("no ETag")
	}
	if early == later {
		t.Error("the ETag did not move between 10:00 and 13:00, but summary.current did — " +
			"a polling dashboard would show a frozen live price until midnight")
	}
}

// Within one slot the tag must NOT move, or the caching is pointless.
func TestETagIsStableWithinASlot(t *testing.T) {
	midnight := londonMidnight(t, pxNow(t))
	rates := make([]float64, 48)
	for i := range rates {
		rates[i] = 20 + float64(i)
	}
	slots := pxSlots(midnight, rates...)
	at := func(h, m int) string {
		s := pxSetup(t, slots)
		s.Clock = fixedClock{midnight.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)}
		return doGET(t, s, "/prices?window=today").Header().Get("ETag")
	}
	if a, b := at(10, 0), at(10, 29); a != b {
		t.Errorf("tag moved inside one half hour (%s vs %s); nothing in the body changed", a, b)
	}
}

// If-None-Match was matched with strings.Contains, so a WEAK validator W/"x"
// satisfied a strong comparison, and any tag merely containing ours did too.
func TestIfNoneMatchRequiresAStrongExactMatch(t *testing.T) {
	s := pxSetup(t, pxSlots(pxNow(t), 10, 11, 12, 13))
	etag := doGET(t, s, "/prices/upcoming?hours=2").Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	for _, tc := range []struct {
		name, hdr string
		want      int
	}{
		{"exact", etag, http.StatusNotModified},
		{"in a list", `"other", ` + etag, http.StatusNotModified},
		{"star", "*", http.StatusNotModified},
		{"weak form of ours", "W/" + etag, http.StatusOK},
		{"ours as a substring", `"x` + etag[1:len(etag)-1] + `y"`, http.StatusOK},
		{"unrelated", `"deadbeef"`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := doGETWithHeader(t, s, "/prices/upcoming?hours=2", "If-None-Match", tc.hdr)
			if req.Code != tc.want {
				t.Errorf("If-None-Match %q -> %d, want %d", tc.hdr, req.Code, tc.want)
			}
		})
	}
}
