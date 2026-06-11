package httpapi

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// Regression: main.go builds the Server via a struct literal (not New()), so
// the unexported `started` field is zero and /healthz + /metrics report a
// garbage started_at / uptime. Start() must stamp `started` when it's unset.
func TestStart_StampsStartedWhenZero(t *testing.T) {
	s := &Server{
		Listen: "127.0.0.1:0",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if !s.started.IsZero() {
		t.Fatalf("precondition: started should be zero for a struct-literal Server")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: Start sets up, then returns via ctx.Done immediately
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if s.started.IsZero() {
		t.Fatal("Start did not stamp started; /healthz and /metrics would report garbage uptime")
	}
}
