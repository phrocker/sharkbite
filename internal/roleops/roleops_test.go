package roleops

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSemanticReadinessReportsDependencies(t *testing.T) {
	deps := NewDependencies("lock", "manager")
	deps.SetStarted(true)
	deps.Set("lock", true, "held")
	handler := Handler(deps, nil)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"name":"manager"`) {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	deps.Set("manager", true, "observed")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("ready response = %d %q", response.Code, response.Body.String())
	}
}

func TestServerShutdownJoinsServeLoop(t *testing.T) {
	server, err := Start("127.0.0.1:0", Handler(NewDependencies(), nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	// Deliberately non-blocking. The assertion is that Done is *already*
	// closed at the moment Shutdown returns, which is the join this function
	// promises. Waiting for the close instead would pass against the old
	// send-then-close implementation too, where Shutdown could return in the
	// gap between the send and the close: the wait would simply observe the
	// close a moment later and report success.
	//
	// This was flaky before because the implementation was racy, not because
	// the assertion was wrong. Now that the serve loop closes Done before
	// Shutdown can return, it is deterministic.
	select {
	case <-server.Done():
	default:
		t.Fatal("shutdown returned before the serve loop ended")
	}
	if err := server.Err(); err != nil {
		t.Fatalf("serve loop error after clean shutdown: %v", err)
	}
}

// TestShutdownAndWatcherBothSeeTheServeError proves the serve error is not
// consumed by whichever caller reads first. The channel previously carried a
// single value, so a goroutine watching Done and a caller inside Shutdown
// raced for it, and the loser observed nil. Both real consumers of this
// package, shoal-tserver and shoal-compactor, watch Done in a goroutine and
// call Shutdown on exit, so a metrics server that failed for a real reason
// could have its error silently dropped.
func TestShutdownAndWatcherBothSeeTheServeError(t *testing.T) {
	server, err := Start("127.0.0.1:0", Handler(NewDependencies(), nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	watched := make(chan error, 1)
	go func() {
		<-server.Done()
		watched <- server.Err()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-watched:
		// A clean shutdown reports no serve error to either observer. The
		// point is that both observed the same thing rather than one of them
		// consuming it.
		if err != nil {
			t.Fatalf("watcher saw %v after a clean shutdown", err)
		}
	case <-ctx.Done():
		t.Fatal("watcher never observed the serve loop ending")
	}
	if err := server.Err(); err != nil {
		t.Fatalf("Err after shutdown = %v", err)
	}
}

// TestShutdownIsIdempotent guards the sync.Once path now that Shutdown reads a
// closed channel rather than draining a value.
func TestShutdownIsIdempotent(t *testing.T) {
	server, err := Start("127.0.0.1:0", Handler(NewDependencies(), nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("second shutdown = %v", err)
	}
	select {
	case <-server.Done():
	default:
		t.Fatal("done must remain readable after repeated shutdown")
	}
}

// failingListener fails every Accept with a sentinel, so http.Server.Serve
// returns something that is not ErrServerClosed and the serve error path is
// actually reachable.
type failingListener struct {
	addr net.Addr
	err  error
}

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (l failingListener) Close() error              { return nil }
func (l failingListener) Addr() net.Addr            { return l.addr }

// TestServeErrorReachesBothObservers is the regression test for the bug that
// motivated this change. The serve error used to be a single buffered value,
// so a goroutine watching Done and a caller inside Shutdown raced for it and
// whichever lost observed nil. Both consumers of this package do exactly that,
// so a metrics server failing for a real reason could have its error silently
// dropped.
//
// A clean shutdown cannot test this: it reports nil to everyone, so the
// assertion would hold even if Err always returned nil. The listener therefore
// fails with a sentinel and both observers must see it.
func TestServeErrorReachesBothObservers(t *testing.T) {
	sentinel := errors.New("listener is gone")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server := serve("127.0.0.1:0",
		failingListener{addr: address, err: sentinel},
		Handler(NewDependencies(), nil))

	watched := make(chan error, 1)
	go func() {
		<-server.Done()
		watched <- server.Err()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Wait for the loop to fail on its own before shutting down. Calling
	// Shutdown first would mark the server closed and Serve would return
	// ErrServerClosed instead, which is normalized to nil and would test
	// nothing.
	select {
	case <-server.Done():
	case <-ctx.Done():
		t.Fatal("serve loop never failed")
	}
	shutdownErr := server.Shutdown(ctx)
	if !errors.Is(shutdownErr, sentinel) {
		t.Fatalf("Shutdown did not surface the serve error: %v", shutdownErr)
	}
	select {
	case observed := <-watched:
		if !errors.Is(observed, sentinel) {
			t.Fatalf("watcher saw %v, want the serve error", observed)
		}
	case <-ctx.Done():
		t.Fatal("watcher never observed the serve loop ending")
	}
	if !errors.Is(server.Err(), sentinel) {
		t.Fatalf("Err after shutdown = %v", server.Err())
	}
}

// TestShutdownJoinsEvenWhenTheContextIsAlreadyDone covers the branch the
// happy-path test cannot reach. With an expired context the select's Done case
// is immediately ready and can win before the serve goroutine has closed its
// channel, so returning there would break the join this function promises.
// Close unblocks Serve, so waiting is bounded rather than a deadlock.
//
// It is reachable in practice: shoal-compactor reuses a shutdown context after
// waiting on another server, by which point it may already be expired.
func TestShutdownJoinsEvenWhenTheContextIsAlreadyDone(t *testing.T) {
	server, err := Start("127.0.0.1:0", Handler(NewDependencies(), nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- server.Shutdown(expired) }()

	// Deliberately no assertion on the returned error. Both select cases can
	// be ready at once and Go picks among them at random, and http.Shutdown
	// returns nil when it finishes closing before the context is consulted, so
	// whether a context cause appears is genuinely nondeterministic here. The
	// property under test is the join.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown blocked instead of joining a closed serve loop")
	}

	select {
	case <-server.Done():
	default:
		t.Fatal("shutdown returned on the context before the serve loop ended")
	}
}
