//go:build integration

package it

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestContainerDBDriverTransactionStartCancellationConformance(t *testing.T) {
	for _, driver := range []string{"postgres", "mysql", "sqlserver"} {
		if !selectedDBForTest(t, driver) {
			continue
		}
		t.Run(driver, func(t *testing.T) {
			cfg := selectedContainerDBConfig(t, driver)
			proxy := newResponseGateProxy(t, net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)))
			proxied := cfg
			proxied.Host, proxied.Port = proxy.hostPort(t)
			db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, proxied))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			pingCtx, pingCancel := context.WithTimeout(t.Context(), 10*time.Second)
			if err := db.PingContext(pingCtx); err != nil {
				pingCancel()
				t.Fatalf("initial ping: %v", err)
			}
			pingCancel()

			proxy.setBlocked(true)
			beginCtx, cancelBegin := context.WithTimeout(t.Context(), 100*time.Millisecond)
			started := time.Now()
			tx, err := db.BeginTx(beginCtx, &sql.TxOptions{})
			cancelBegin()
			if tx != nil {
				_ = tx.Rollback()
				t.Fatal("transaction start succeeded while server response was blackholed")
			}
			if err == nil {
				t.Fatal("transaction start error=nil")
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("transaction start cancellation took %s", elapsed)
			}

			proxy.setBlocked(false)
			pingCtx, pingCancel = context.WithTimeout(t.Context(), 10*time.Second)
			if err := db.PingContext(pingCtx); err != nil {
				pingCancel()
				t.Fatalf("replacement connection ping: %v", err)
			}
			pingCancel()
			if proxy.accepted.Load() < 2 {
				t.Fatalf("canceled transaction connection was reused; accepted connections=%d", proxy.accepted.Load())
			}
		})
	}
}

func TestContainerDBDriverOracleTransactionStartIsImmediateAndRollbackable(t *testing.T) {
	if !selectedDBForTest(t, "oracle") {
		return
	}
	cfg := selectedContainerDBConfig(t, "oracle")
	db, err := sql.Open(sqlDriverName("oracle"), integrationDBDSN("oracle", cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Oracle BeginTx performed blocking network work: %s", elapsed)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	db.SetMaxOpenConns(1)
	for iteration := range 100 {
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Fatalf("iteration %d: acquire dedicated connection: %v", iteration, err)
		}
		execCtx, cancelBegin := context.WithCancel(t.Context())
		beginCtx := context.WithoutCancel(execCtx)
		barrier := make(chan struct{})
		type beginResult struct {
			tx      *sql.Tx
			err     error
			elapsed time.Duration
		}
		result := make(chan beginResult, 1)
		go func() {
			<-barrier
			started := time.Now()
			tx, err := conn.BeginTx(beginCtx, &sql.TxOptions{})
			result <- beginResult{tx: tx, err: err, elapsed: time.Since(started)}
		}()
		canceled := make(chan struct{})
		go func() {
			<-barrier
			cancelBegin()
			close(canceled)
		}()
		close(barrier)
		<-canceled
		got := <-result
		if got.elapsed > 500*time.Millisecond {
			t.Fatalf("iteration %d: Oracle BeginTx/cancel race took %s", iteration, got.elapsed)
		}
		if got.tx == nil {
			t.Fatalf("iteration %d: detached Oracle BeginTx failed: %v", iteration, got.err)
		} else {
			if got.err != nil {
				t.Fatalf("iteration %d: tx returned with error=%v", iteration, got.err)
			}
			if err := got.tx.Rollback(); err != nil {
				t.Fatalf("iteration %d: rollback error=%v", iteration, err)
			}
		}
		if err := conn.PingContext(t.Context()); err != nil {
			t.Fatalf("iteration %d: detached Oracle transaction poisoned its connection: %v", iteration, err)
		}
		_ = conn.Close()
		pingCtx, cancelPing := context.WithTimeout(t.Context(), time.Second)
		if err := db.PingContext(pingCtx); err != nil {
			cancelPing()
			t.Fatalf("iteration %d: Oracle connection was not released after BeginTx/cancel race: %v", iteration, err)
		}
		cancelPing()
	}
}

type responseGateProxy struct {
	listener        net.Listener
	target          string
	blocked         bool
	responseWaiting bool
	closed          bool
	mu              sync.Mutex
	cond            *sync.Cond
	accepted        atomic.Int64
}

func newResponseGateProxy(t *testing.T, target string) *responseGateProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &responseGateProxy{listener: listener, target: target}
	p.cond = sync.NewCond(&p.mu)
	go p.acceptLoop()
	t.Cleanup(func() {
		p.mu.Lock()
		p.closed = true
		p.blocked = false
		p.cond.Broadcast()
		p.mu.Unlock()
		_ = listener.Close()
	})
	return p
}

func (p *responseGateProxy) hostPort(t *testing.T) (string, string) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(p.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return host, rawPort
}

func (p *responseGateProxy) setBlocked(blocked bool) {
	p.mu.Lock()
	p.blocked = blocked
	p.responseWaiting = false
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *responseGateProxy) copyClientToServer(server, client net.Conn) {
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := client.Read(buffer)
		if n > 0 {
			if _, err := server.Write(buffer[:n]); err != nil {
				return
			}
			// SQL Server cancels an in-flight request with a second TDS packet
			// and waits for the server's attention acknowledgement. Continue to
			// blackhole the original BEGIN response until that cancel packet is
			// forwarded, then let the acknowledgement through. Drivers that
			// cancel by closing the socket never take this path.
			p.mu.Lock()
			if p.blocked && p.responseWaiting {
				p.blocked = false
				p.cond.Broadcast()
			}
			p.mu.Unlock()
		}
		if readErr != nil {
			return
		}
	}
}

func (p *responseGateProxy) acceptLoop() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.DialTimeout("tcp", p.target, 5*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.accepted.Add(1)
		go func() {
			defer client.Close()
			defer server.Close()
			done := make(chan struct{})
			go func() {
				p.copyClientToServer(server, client)
				close(done)
			}()
			buffer := make([]byte, 32<<10)
			for {
				n, readErr := server.Read(buffer)
				if n > 0 {
					p.mu.Lock()
					p.responseWaiting = p.blocked
					for p.blocked && !p.closed {
						p.cond.Wait()
					}
					p.responseWaiting = false
					closed := p.closed
					p.mu.Unlock()
					if closed {
						return
					}
					if _, err := client.Write(buffer[:n]); err != nil {
						return
					}
				}
				if readErr != nil {
					return
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}
}
