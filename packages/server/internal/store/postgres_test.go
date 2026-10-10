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

func TestUserAndTokenControlValidationIncludesUnicode(t *testing.T) {
	for _, value := range []string{"a\x00b", "a\tb", "a\nb", "a\x7fb", "a\u0085b", "a\u009bb"} {
		if !containsControl(value) {
			t.Errorf("accepted control characters in %q", value)
		}
		// Validation must complete before any database access.
		p := &Postgres{}
		if _, err := p.CreateUser(context.Background(), value, "", "member"); err == nil {
			t.Errorf("accepted user name %q", value)
		}
		if _, _, err := p.CreateToken(context.Background(), "user", value); err == nil {
			t.Errorf("accepted token name %q", value)
		}
	}
	for _, value := range []string{"普通用户", "O'Connor", "Lab member"} {
		if containsControl(value) {
			t.Errorf("rejected ordinary display name %q", value)
		}
	}
}
