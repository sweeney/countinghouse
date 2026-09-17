package collector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/notify"
	"github.com/sweeney/countinghouse/internal/prices"
)

// unreadableStore wraps a real store and fails Range after a chosen number of
// calls, so a sync can succeed at fetching and then fail at assessing.
type unreadableStore struct {
	prices.Store
	failRange bool
}

func (u *unreadableStore) Range(ctx context.Context, code string, from, to time.Time) ([]prices.Slot, error) {
	if u.failRange {
		return nil, errors.New("disk I/O error")
	}
	return u.Store.Range(ctx, code, from, to)
}

// A sync that stored prices and then could NOT READ the archive must not report
// success. It did: assess alerted and returned, Sync called markSuccess
// unconditionally, LastError was cleared and CompleteTo kept its previous value —
// so /healthz reported ok for an archive it cannot read.
//
// The same failure dayCompleteness was fixed for, dropped one frame up.
func TestSyncDoesNotMarkSuccessWhenTheArchiveIsUnreadable(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-09T23:00:00Z"), ts(t, "2026-09-10T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-09-10T16:10:00Z"), f)
	ctx := context.Background()

	// One good sync first, so LastSuccess and CompleteTo are populated.
	if _, err := h.c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if h.c.Status().LastError != "" {
		t.Fatal("expected a clean first sync")
	}

	// Now the archive becomes unreadable.
	bad := &unreadableStore{Store: h.store, failRange: true}
	h.c.store = bad
	h.clock.Advance(time.Hour)

	_, err := h.c.Sync(ctx)
	if err == nil {
		t.Error("Sync reported success while the archive was unreadable")
	}
	st := h.c.Status()
	if st.LastError == "" {
		t.Error("LastError is empty; /healthz will report ok for an unreadable archive")
	}
	if !h.noti.has(notify.KindArchiveUnreadable) {
		t.Errorf("no archive_unreadable alert: %v", h.noti.kinds())
	}
}
