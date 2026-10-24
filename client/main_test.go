package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunWithReconnectRetriesUntilConnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	err := runWithReconnect(ctx, time.Millisecond, 2*time.Millisecond, func(context.Context) error {
		switch calls.Add(1) {
		case 1, 2:
			return errors.New("temporary failure")
		default:
			cancel()
			return nil
		}
	})
	if err != nil {
		t.Fatalf("runWithReconnect() error = %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("connect called %d times, want 3", got)
	}
}

func TestRunWithReconnectStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	err := runWithReconnect(ctx, time.Hour, time.Hour, func(context.Context) error {
		calls.Add(1)
		cancel()
		return errors.New("temporary failure")
	})
	if err != nil {
		t.Fatalf("runWithReconnect() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("connect called %d times, want 1", got)
	}
}

func TestRunWithReconnectRejectsInvalidBackoff(t *testing.T) {
	if err := runWithReconnect(context.Background(), time.Second, time.Millisecond, func(context.Context) error { return nil }); err == nil {
		t.Fatal("runWithReconnect() accepted max backoff below initial backoff")
	}
}
