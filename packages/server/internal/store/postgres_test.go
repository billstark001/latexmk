package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestOpenRespectsDeadlineWhileServerDoesNotHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		<-stop
	}()
	defer func() { close(stop); _ = listener.Close(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	db, err := Open(ctx, fmt.Sprintf("postgres://test@%s/test?sslmode=disable&connect_timeout=1", listener.Addr()))
	if db != nil {
		db.Close()
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Open ignored caller deadline for %v: %v", elapsed, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
}
