// Package server is the server-construction slice of implementation
// package 4 (section 7, "Server and bounded shutdown", minus the lifecycle).
//
// It builds the *http.Server the service serves from: the four explicit
// connection timeouts from package 1's configuration (the unconfigured
// default server is never used), the serve root context as BaseContext,
// and the fully assembled router from internal/api. The migrations-
// converged state is carried in-process: the startup migration step
// (package 1) publishes it through MarkMigrationsConverged, and the
// /readyz route reads it through the router's api.Deps — readiness never
// reads migration history (the runtime role has no grant on
// vector_control.schema_migrations).
//
// Serving, bounded shutdown, and the command entrypoint are not part of
// this slice (section 7's lifecycle owns the serve root's cancel, the
// process-level shutdown marker, and the drain; a later package consumes
// this server).
package server

import (
	"context"
	"net"
	"net/http"

	"vector-service/internal/api"
	"vector-service/internal/config"
)

// DefaultBaseContext is the default serve root context for a new Server.
// It is context.Background() (not a WithCancel pair): the bounded-shutdown
// lifecycle (section 7) creates its own cancellable serve root and swaps it
// in with NewWithRoot. The default is what unit tests and non-lifecycle
// constructors serve from.
func DefaultBaseContext() context.Context { return context.Background() }

// Server is the constructed HTTP server: the four explicit connection
// timeouts, the serve root context (BaseContext), the assembled router,
// and the in-process migrations-converged state.
type Server struct {
	// httpServer is the constructed *http.Server. It is the value the
	// bounded-shutdown lifecycle (section 7) hands to Serve and
	// Server.Shutdown.
	httpServer *http.Server
	// deps is the shared dependency set. NewWithRoot copies the caller's
	// value into *deps and hands *deps to the router, so both the router
	// and MarkMigrationsConverged observe the same memory: the converged
	// flag (Deps.MigrationsApplied) is dynamic and stays live after
	// construction.
	deps *api.Deps
}

// New builds the server: the router is assembled from the validated
// configuration and the dependency set (NewRouter), the *http.Server takes
// the four explicit connection timeouts from the configuration, and the
// serve root context becomes its BaseContext.
func New(cfg *config.Config, deps api.Deps, root context.Context) *Server {
	return NewWithRoot(cfg, deps, root)
}

// NewWithRoot builds the server with an explicit serve root context. It
// is the form the bounded-shutdown lifecycle uses (the root is the
// lifecycle's cancellable context.WithCancel(context.Background())).
func NewWithRoot(cfg *config.Config, deps api.Deps, root context.Context) *Server {
	if root == nil {
		root = context.Background()
	}
	// deps is stored by pointer and handed to the router by pointer: the
	// readyz route reads MigrationsApplied at request time through this
	// memory, so MarkMigrationsConverged (called after construction, once
	// the startup migration step has converged) is observed by the
	// already-built router without rebuilding it.
	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           api.NewRouterShared(cfg, &deps),
			BaseContext:       func(_ net.Listener) context.Context { return root },
			ReadHeaderTimeout: cfg.ServerReadHeaderTimeout,
			ReadTimeout:       cfg.ServerReadTimeout,
			WriteTimeout:      cfg.ServerWriteTimeout,
			IdleTimeout:       cfg.ServerIdleTimeout,
		},
		deps: &deps,
	}
}

// MarkMigrationsConverged records the startup migration step's convergence
// result. It is the package-1 startup's hook: after the runner succeeds
// (the database is converged) it is called once, before the listener
// opens. Subsequent calls are idempotent no-ops (convergence is terminal
// for the process's lifetime — a converged state never reverts to
// unconverged; a process that fails to converge never calls this at all,
// it exits non-zero before listening).
func (s *Server) MarkMigrationsConverged() {
	s.deps.MigrationsApplied = func() bool { return true }
}

// HTTPServer returns the constructed *http.Server (the value Serve and
// Server.Shutdown operate on). The lifecycle (section 7) owns the serve
// root's cancel and the drain; this server only exposes the constructed
// value.
func (s *Server) HTTPServer() *http.Server { return s.httpServer }
