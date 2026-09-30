package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"vector-service/internal/api"
	"vector-service/internal/config"
)

// shutdownTestConfig returns a configuration with short, distinct shutdown
// bounds so the lifecycle's two phases (grace drain, bounded close) can be
// exercised without real waiting.
func shutdownTestConfig() *config.Config {
	return &config.Config{
		ListenAddr:               "127.0.0.1:0",
		HTTPShutdownGrace:        500 * time.Millisecond,
		HTTPShutdownCloseTimeout: 1 * time.Second,
		ServerReadHeaderTimeout:  time.Second,
		ServerReadTimeout:        2 * time.Second,
		ServerWriteTimeout:       2 * time.Second,
		ServerIdleTimeout:        time.Second,
	}
}

// blockingHandler is a request handler that writes nothing until its request
// context becomes inactive. It stands in for a genuinely in-flight request:
// while the serve root is active it holds the connection open (so
// Server.Shutdown cannot complete the drain until the root is canceled), and
// once the root is canceled it returns, allowing the drain to finish.
func blockingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}
}

// testServer returns a Server with the supplied handler configured. The
// listener (when any) is held by the caller and passed to the controller seam
// (NewShutdownControllerForSignals), which serves it directly.
func testServer(cfg *config.Config, h http.Handler) *Server {
	srv := New(cfg, api.Deps{}, context.Background())
	srv.HTTPServer().Handler = h
	return srv
}

// freshListener returns a fresh loopback listener (127.0.0.1:0) the test
// serves on and closes on cleanup. The test holds it for the duration so no
// other bind can collide.
func freshListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return l
}

// serveController runs Serve (built for an injected signal channel) in a
// goroutine and returns the serve-error channel and the signal channel the
// test delivers on. The signal channel is the one the test injected (the
// controller's sigSrc returns exactly that channel), so the test already
// holds it — no polling of controller state is needed, which keeps the test
// race-free.
//
// The signal is delivered through the controller's real signal channel (the
// production Serve/select/shutdown path) rather than by self-sending a real OS
// signal: self-sending is unreliable to repeat within one process (the test
// binary launches with SIGINT/SIGTERM in SIG_IGN mode, and after the first
// handler round-trip the runtime drops subsequent self-sent signals), which
// would make sequential tests hang. Driving the channel directly is
// deterministic, race-free, and exercises the identical code path.
func serveController(t *testing.T, c *ShutdownController, signalCh chan os.Signal) (serveErr <-chan error) {
	t.Helper()
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- c.Serve(context.Background()) }()
	return serveErrCh
}

// deliverAndAwait sends sig on the signal channel (what a registered handler
// would receive) and blocks until Serve returns its outcome.
func deliverAndAwait(t *testing.T, serveErr <-chan error, signalCh chan os.Signal, sig syscall.Signal, bound time.Duration) error {
	t.Helper()
	signalCh <- sig
	select {
	case err := <-serveErr:
		return err
	case <-time.After(bound):
		t.Fatalf("Serve did not return within %s after %s", bound, sig)
		return nil
	}
}

// ---------------------------------------------------------------------------
// Exit outcome on the first signal
// ---------------------------------------------------------------------------

func TestServe_FirstSignal_PoolCloseInTime(t *testing.T) {
	cfg := shutdownTestConfig()
	l := freshListener(t)
	defer l.Close()

	closed := make(chan struct{})
	closer := &scriptCloser{onClose: func() { close(closed) }}
	srv := testServer(cfg, blockingHandler())
	signalCh := make(chan os.Signal, 2)
	c := NewShutdownControllerForSignals(cfg, srv, closer, signalCh, l)
	serveErr := serveController(t, c, signalCh)
	if err := deliverAndAwait(t, serveErr, signalCh, syscall.SIGINT, 3*time.Second); err != nil {
		t.Fatalf("Serve returned %v, want nil (pool closed in time)", err)
	}
	awaitClosed(t, closed, "pool was not closed after the signal")
}

func TestServe_FirstSignal_PoolCloseTimeout(t *testing.T) {
	cfg := shutdownTestConfig()
	l := freshListener(t)
	defer l.Close()

	// A pool whose Close never returns (a held lease past the bound): the
	// bounded close must yield ErrPoolCloseTimeout.
	block := make(chan struct{})
	defer close(block)
	closer := &scriptCloser{block: block}
	srv := testServer(cfg, blockingHandler())
	signalCh := make(chan os.Signal, 2)
	c := NewShutdownControllerForSignals(cfg, srv, closer, signalCh, l)
	serveErr := serveController(t, c, signalCh)
	err := deliverAndAwait(t, serveErr, signalCh, syscall.SIGTERM, 3*time.Second)
	if !IsPoolCloseTimeout(err) {
		t.Fatalf("Serve returned %v, want the pool-close-timeout outcome", err)
	}
}

func TestServe_FirstSignal_NilPool(t *testing.T) {
	cfg := shutdownTestConfig()
	l := freshListener(t)
	defer l.Close()

	srv := testServer(cfg, blockingHandler())
	signalCh := make(chan os.Signal, 2)
	c := NewShutdownControllerForSignals(cfg, srv, nil, signalCh, l)
	serveErr := serveController(t, c, signalCh)
	if err := deliverAndAwait(t, serveErr, signalCh, syscall.SIGINT, 3*time.Second); err != nil {
		t.Fatalf("Serve returned %v, want nil (nil pool is a no-op)", err)
	}
}

// ---------------------------------------------------------------------------
// Repeated signals are ignored
// ---------------------------------------------------------------------------

func TestServe_SecondSignalIgnored(t *testing.T) {
	cfg := shutdownTestConfig()
	l := freshListener(t)
	defer l.Close()

	closed := make(chan struct{})
	closer := &scriptCloser{onClose: func() { close(closed) }}
	srv := testServer(cfg, blockingHandler())
	signalCh := make(chan os.Signal, 2)
	c := NewShutdownControllerForSignals(cfg, srv, closer, signalCh, l)
	serveErr := serveController(t, c, signalCh)

	// First signal starts the shutdown; a second signal during the drain must
	// be ignored (it only queues on the buffered channel).
	signalCh <- syscall.SIGINT
	time.Sleep(30 * time.Millisecond)
	signalCh <- syscall.SIGINT // second signal: queued, never drained

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after the signals")
	}
	awaitClosed(t, closed, "pool was not closed")
}

// ---------------------------------------------------------------------------
// Startup failure (no drain)
// ---------------------------------------------------------------------------

func TestServe_StartupFailure_ClosesPoolAndReturnsError(t *testing.T) {
	cfg := shutdownTestConfig()
	// Bind a real port, then serve that address with NO listener: the server's
	// own ListenAndServe tries to re-bind it and fails (the startup-failure,
	// no-drain path).
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer hold.Close()
	badAddr := hold.Addr().String()

	closed := make(chan struct{})
	closer := &scriptCloser{onClose: func() { close(closed) }}
	srv := testServer(cfg, blockingHandler())
	srv.HTTPServer().Addr = badAddr
	c := NewShutdownControllerForSignals(cfg, srv, closer, make(chan os.Signal, 2), nil)

	// The pre-bound listener is held, so ListenAndServe fails to bind (the
	// startup-failure, no-drain path). Serve must close the pool directly and
	// return the bind error. No signal is delivered.
	err = c.Serve(context.Background())
	if err == nil {
		t.Fatal("Serve returned nil, want a startup failure")
	}
	awaitClosed(t, closed, "pool was not closed on the startup-failure path")
}

// ---------------------------------------------------------------------------
// External abort before any signal
// ---------------------------------------------------------------------------

func TestServe_ContextDoneBeforeSignal_ClosesPoolReturnsNil(t *testing.T) {
	cfg := shutdownTestConfig()
	l := freshListener(t)
	defer l.Close()

	closed := make(chan struct{})
	closer := &scriptCloser{onClose: func() { close(closed) }}
	srv := testServer(cfg, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c := NewShutdownControllerForSignals(cfg, srv, closer, make(chan os.Signal, 2), l)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := c.Serve(ctx); err != nil {
		t.Fatalf("Serve returned %v, want nil on external abort", err)
	}
	awaitClosed(t, closed, "pool was not closed on the external-abort path")
}

// ---------------------------------------------------------------------------
// scriptCloser: a scripted poolCloser for the bounded-close seam
// ---------------------------------------------------------------------------

// scriptCloser is a scripted poolCloser. If block is non-nil, Close blocks
// until block is closed (models a held lease past the close bound). If onClose
// is set it is invoked when Close runs (records that the close happened).
// Close is safe to call multiple times (idempotent).
type scriptCloser struct {
	block   chan struct{}
	onClose func()
}

func (s *scriptCloser) Close() {
	if s.block != nil {
		<-s.block
		return
	}
	if s.onClose != nil {
		s.onClose()
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// awaitClosed blocks until closed is closed or the bound elapses, fataling on
// the bound.
func awaitClosed(t *testing.T, closed chan struct{}, msg string) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal(msg)
	}
}

// ---------------------------------------------------------------------------
// IsPoolCloseTimeout
// ---------------------------------------------------------------------------

func TestIsPoolCloseTimeout(t *testing.T) {
	if IsPoolCloseTimeout(nil) {
		t.Fatal("IsPoolCloseTimeout(nil) = true, want false")
	}
	if IsPoolCloseTimeout(errors.New("other")) {
		t.Fatal("IsPoolCloseTimeout(other) = true, want false")
	}
	if !IsPoolCloseTimeout(&ErrPoolCloseTimeout{Timeout: time.Second}) {
		t.Fatal("IsPoolCloseTimeout(timeout) = false, want true")
	}
}
