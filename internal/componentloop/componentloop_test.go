package componentloop

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/fleetlyrun/fleetly/internal/state"
)

func TestSleepCtx(t *testing.T) {
	if !SleepCtx(context.Background(), 0) {
		t.Fatal("d<=0 on live ctx: want immediate true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if SleepCtx(ctx, time.Minute) {
		t.Fatal("cancelled ctx: want false")
	}
	if SleepCtx(ctx, 0) {
		t.Fatal("cancelled ctx with d<=0: want false")
	}
	if !SleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("short sleep on live ctx: want true")
	}
}

func TestRetryOrScan(t *testing.T) {
	cases := []struct {
		retry, scan time.Duration
		converged   bool
		want        time.Duration
	}{
		{retry: 5 * time.Second, scan: time.Minute, converged: true, want: time.Minute},
		{retry: 5 * time.Second, scan: time.Minute, converged: false, want: 5 * time.Second},
		{retry: 5 * time.Second, scan: 0, converged: true, want: 5 * time.Second},
	}
	for _, c := range cases {
		if got := RetryOrScan(c.retry, c.scan, c.converged); got != c.want {
			t.Errorf("RetryOrScan(%v, %v, %v) = %v, want %v", c.retry, c.scan, c.converged, got, c.want)
		}
	}
}

func TestSameStrings(t *testing.T) {
	if !SameStrings(nil, nil) || !SameStrings([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("equal slices: want true")
	}
	if SameStrings([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("order-sensitive: swapped order must compare false")
	}
	if SameStrings([]string{"a"}, []string{"a", "b"}) || SameStrings([]string{"a", "b"}, nil) {
		t.Error("length mismatch: want false")
	}
}

func TestResolveNetworkNames(t *testing.T) {
	resolve := func(_ context.Context, target string) (string, error) {
		return "name-of-" + target, nil
	}
	targets := []string{"id1", "id2"}
	if err := ResolveNetworkNames(context.Background(), targets, resolve); err != nil {
		t.Fatalf("ResolveNetworkNames: %v", err)
	}
	if !SameStrings(targets, []string{"name-of-id1", "name-of-id2"}) {
		t.Errorf("in-place resolution: got %v", targets)
	}
	boom := func(context.Context, string) (string, error) { return "", context.Canceled }
	if err := ResolveNetworkNames(context.Background(), targets, boom); err == nil {
		t.Error("resolver error must propagate (caller backs off, no false drift)")
	}
}

func TestEmitEvent(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	log := slog.New(slog.DiscardHandler)
	// 事件名须为 eventcode 注册名（state.AppendEvent 只收注册码——
	// ADR-0011 只增注册表）；借 metrics 的注册码验证骨架本体。
	EmitEvent(context.Background(), st, log, "testcomp", "metrics.stack_deployed", "platform:test", map[string]string{
		"service": "svc", "reason": "created",
	})

	evs, err := st.EventsSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events appended: got %d, want 1", len(evs))
	}
	if evs[0].Name != "metrics.stack_deployed" || evs[0].Subject != "platform:test" {
		t.Errorf("first event: got %q/%q", evs[0].Name, evs[0].Subject)
	}
	if evs[0].Payload != `{"reason":"created","service":"svc"}` && evs[0].Payload != `{"service":"svc","reason":"created"}` {
		t.Errorf("first payload: got %q", evs[0].Payload)
	}
}
