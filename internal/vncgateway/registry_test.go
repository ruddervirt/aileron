package vncgateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRegistryConnectThenJoinAndClose(t *testing.T) {
	tunnelCalls := 0
	reg := newRegistry(func(context.Context, string, string) (int, error) {
		tunnelCalls++
		return 12345, nil
	}, quietLogger())
	ctx := context.Background()

	first, err := reg.Acquire(ctx, "ns", "vmi")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !first.primary || first.port != 12345 {
		t.Fatalf("first = %+v, want primary on port 12345", first)
	}
	if tunnelCalls != 1 {
		t.Fatalf("tunnelCalls = %d, want 1", tunnelCalls)
	}

	// Second client arrives before the primary opened: it must wait, then join.
	joinCh := make(chan acquireResult, 1)
	go func() {
		r, _ := reg.Acquire(ctx, "ns", "vmi")
		joinCh <- r
	}()

	reg.PrimaryOpened("ns/vmi", "$abc")

	select {
	case second := <-joinCh:
		if second.primary || second.connID != "$abc" {
			t.Fatalf("second = %+v, want join on $abc", second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("join did not resolve after primary opened")
	}
	if tunnelCalls != 1 {
		t.Fatalf("join must not create another tunnel; tunnelCalls = %d", tunnelCalls)
	}
	reg.JoinOpened("ns/vmi")

	if reg.Size() != 1 || !reg.Has("ns", "vmi") {
		t.Fatalf("expected one live session")
	}

	// Primary leaves; the joiner keeps the session alive.
	reg.Closed("ns/vmi", false)
	if reg.Size() != 1 {
		t.Fatalf("session must survive primary leaving; size = %d", reg.Size())
	}
	third, err := reg.Acquire(ctx, "ns", "vmi")
	if err != nil || third.connID != "$abc" {
		t.Fatalf("third = %+v err %v, want join on $abc", third, err)
	}

	// Last client leaves; session dropped; next acquire reconnects.
	reg.Closed("ns/vmi", false)
	if reg.Size() != 0 {
		t.Fatalf("size = %d, want 0 after last client", reg.Size())
	}
	fourth, err := reg.Acquire(ctx, "ns", "vmi")
	if err != nil || !fourth.primary {
		t.Fatalf("fourth = %+v err %v, want fresh primary", fourth, err)
	}
	if tunnelCalls != 2 {
		t.Fatalf("tunnelCalls = %d, want 2", tunnelCalls)
	}
}

func TestRegistryGuacdErrorDropsSession(t *testing.T) {
	reg := newRegistry(func(context.Context, string, string) (int, error) { return 12345, nil }, quietLogger())
	ctx := context.Background()

	if _, err := reg.Acquire(ctx, "ns", "vmi"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	reg.PrimaryOpened("ns/vmi", "$abc")
	reg.JoinOpened("ns/vmi")

	reg.Closed("ns/vmi", true) // guacd error: connection-scoped, drop everything
	if reg.Size() != 0 {
		t.Fatalf("session must drop on guacd error; size = %d", reg.Size())
	}

	retry, err := reg.Acquire(ctx, "ns", "vmi")
	if err != nil || !retry.primary {
		t.Fatalf("retry = %+v err %v, want fresh primary", retry, err)
	}
}

func TestRegistryPrimaryFailureRejectsWaiters(t *testing.T) {
	reg := newRegistry(func(context.Context, string, string) (int, error) { return 12345, nil }, quietLogger())
	ctx := context.Background()

	if _, err := reg.Acquire(ctx, "ns", "vmi"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	joinErr := make(chan error, 1)
	go func() {
		_, err := reg.Acquire(ctx, "ns", "vmi")
		joinErr <- err
	}()
	// Let the joiner park on the session's ready channel before the primary
	// fails — only waiters present at failure time are rejected (a client that
	// arrives after the session is dropped correctly becomes a fresh primary).
	time.Sleep(200 * time.Millisecond)

	reg.AttachFailed("ns/vmi", true, errors.New("guacd refused"))

	select {
	case err := <-joinErr:
		if err == nil || !strings.Contains(err.Error(), "guacd refused") {
			t.Fatalf("join err = %v, want 'guacd refused'", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not rejected")
	}
	if reg.Size() != 0 {
		t.Fatalf("size = %d, want 0", reg.Size())
	}
}

func TestRegistryTunnelFailureClearsSlot(t *testing.T) {
	fail := true
	reg := newRegistry(func(context.Context, string, string) (int, error) {
		if fail {
			return 0, errors.New("bridge down")
		}
		return 999, nil
	}, quietLogger())
	ctx := context.Background()

	if _, err := reg.Acquire(ctx, "ns", "vmi"); err == nil || !strings.Contains(err.Error(), "bridge down") {
		t.Fatalf("acquire err = %v, want 'bridge down'", err)
	}
	if reg.Size() != 0 {
		t.Fatalf("size = %d, want 0 after tunnel failure", reg.Size())
	}

	fail = false
	ok, err := reg.Acquire(ctx, "ns", "vmi")
	if err != nil || ok.port != 999 {
		t.Fatalf("ok = %+v err %v, want port 999", ok, err)
	}
}
