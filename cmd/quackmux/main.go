// Command quackmux is a multiplexing gateway for DuckDB's Quack protocol.
//
//	quackmux serve -config /etc/quackmux/config.yaml
//	quackmux validate -config config.yaml
//	quackmux token            # generate a token and print its sha256 for users.yaml
//	quackmux hash-token [TOKEN] # print the sha256 of an existing token (read from stdin without TOKEN)
//	quackmux version
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	kclient "k8s.io/client-go/kubernetes"

	"quackmux/internal/admin"
	"quackmux/internal/auth"
	"quackmux/internal/config"
	"quackmux/internal/engine"
	"quackmux/internal/engine/kubernetes"
	"quackmux/internal/engine/process"
	"quackmux/internal/engine/static"
	"quackmux/internal/ha"
	"quackmux/internal/metrics"
	"quackmux/internal/proxy"
	"quackmux/internal/session"
	"quackmux/internal/upstream"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "token":
		err = cmdToken()
	case "hash-token":
		err = cmdHashToken(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("quackmux", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "quackmux:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `quackmux - multiplexing gateway for DuckDB Quack

Usage:
  quackmux serve -config FILE      run the gateway
  quackmux validate -config FILE   check a configuration file
  quackmux token                   generate a client token and its sha256
  quackmux hash-token [TOKEN]      print the sha256 of a token (for users.yaml); without
                                   TOKEN it is read from stdin, which keeps it out of
                                   the process list and the shell history
  quackmux version
`)
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	path := fs.String("config", "config.yaml", "configuration file")
	_ = fs.Parse(args)
	if _, err := config.Load(*path); err != nil {
		return err
	}
	fmt.Println("configuration is valid")
	return nil
}

func cmdToken() error {
	tok, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	fmt.Printf("token:  %s\nsha256: %s\n", tok, auth.HashToken(tok))
	return nil
}

func newLogger(c config.LogConfig) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

// sessionKey loads the secret that seals connection ids.
func sessionKey(cfg *config.Config, log *slog.Logger) ([]byte, error) {
	if v := strings.TrimSpace(os.Getenv("QUACKMUX_SESSION_KEY")); v != "" {
		return []byte(v), nil
	}
	if cfg.Server.SessionKeyFile != "" {
		b, err := os.ReadFile(cfg.Server.SessionKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read session key: %w", err)
		}
		if k := strings.TrimSpace(string(b)); k != "" {
			return []byte(k), nil
		}
		return nil, fmt.Errorf("session key file %s is empty", cfg.Server.SessionKeyFile)
	}
	log.Warn("no session key configured (server.sessionKeyFile / QUACKMUX_SESSION_KEY): sessions will not survive a gateway restart")
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// cmdHashToken prints the sha256 of a token given as the argument or, to
// keep it out of process listings and shell histories, on stdin.
func cmdHashToken(args []string) error {
	var tok string
	switch len(args) {
	case 1:
		tok = args[0]
	case 0:
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
		if err != nil {
			return err
		}
		tok = strings.TrimSpace(string(b))
		if tok == "" {
			return errors.New("hash-token: no token on stdin")
		}
	default:
		return errors.New("usage: quackmux hash-token [TOKEN]")
	}
	if strings.Contains(tok, "?") {
		// The gateway hashes only the part before '?' (client options).
		return errors.New("hash-token: a token must not contain '?'")
	}
	fmt.Println(auth.HashToken(tok))
	return nil
}

// placeholderMarker marks the example secrets shipped in
// deploy/kubernetes/base/secrets; they are public.
const placeholderMarker = "CHANGE-ME-INSECURE-PLACEHOLDER"

// refusePlaceholder fails for a secret that still is one of the shipped,
// public examples, except with auth.mode none (development), where it only
// warns.
func refusePlaceholder(cfg *config.Config, log *slog.Logger, what, value string) error {
	if !strings.Contains(value, placeholderMarker) {
		return nil
	}
	if cfg.Auth.Mode == "none" {
		log.Warn("the " + what + " is the public example from the repository; replace it outside development")
		return nil
	}
	return fmt.Errorf("the %s is the public example from the repository (%s...): generate one with \"quackmux token\"", what, placeholderMarker)
}

func readTokenFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "/etc/quackmux/config.yaml", "configuration file")
	_ = fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log)
	log.Info("starting quackmux", "version", version, "provider", cfg.Provider.Type, "auth", cfg.Auth.Mode)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	met := metrics.New()
	key, err := sessionKey(cfg, log)
	if err == nil {
		err = refusePlaceholder(cfg, log, "session key", string(key))
	}
	if err != nil {
		return err
	}
	sealer, err := session.NewSealer(key)
	if err != nil {
		return err
	}
	authn, err := auth.New(cfg.Auth, log)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	defer authn.Close()
	if rl, ok := authn.(auth.Reloader); ok {
		// SIGHUP re-reads the users file immediately (it is also polled).
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
		go func() {
			for range hup {
				if err := rl.Reload(); err != nil {
					log.Error("reloading authentication data failed, keeping the previous state", "err", err)
				} else {
					log.Info("authentication data reloaded")
				}
			}
		}()
	}

	var prov engine.Provider
	var kube kclient.Interface
	var namespace string
	switch cfg.Provider.Type {
	case "kubernetes":
		kube, _, err = kubernetes.NewClient(cfg.Provider.Kubernetes)
		if err != nil {
			return fmt.Errorf("kubernetes client: %w", err)
		}
		kp, err := kubernetes.New(cfg, kube, log)
		if err != nil {
			return err
		}
		namespace = kp.Namespace()
		prov = kp
	case "process":
		if prov, err = process.New(cfg, log); err != nil {
			return err
		}
	case "static":
		if prov, err = static.New(cfg, log); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown provider %q", cfg.Provider.Type)
	}

	up := upstream.New(cfg.Server.Upstream)
	engines := engine.NewManager(cfg, prov, log, met)
	sessions := session.NewManager(cfg, engines, up, sealer, log, met)
	engines.SetSessionCloser(sessions)

	var leading atomic.Bool
	var started atomic.Bool
	ready := func() (bool, string) {
		if cfg.HA.Enabled && !leading.Load() {
			return false, "not the leader"
		}
		if !started.Load() || !engines.Synced() {
			return false, "starting up"
		}
		return true, ""
	}
	gw := proxy.New(proxy.Options{Config: cfg, Auth: authn, Engines: engines, Sessions: sessions, Upstream: up, Log: log, Metrics: met, Ready: ready})

	adminToken, err := readTokenFile(cfg.Admin.TokenFile)
	if err != nil {
		return fmt.Errorf("admin token: %w", err)
	}
	if cfg.Admin.TokenFile != "" && adminToken == "" {
		// An empty file (a failed "quackmux token > file") must not
		// silently open the admin API.
		return fmt.Errorf("admin token file %s is empty", cfg.Admin.TokenFile)
	}
	if err := refusePlaceholder(cfg, log, "admin token", adminToken); err != nil {
		return err
	}
	if cfg.Admin.Listen != "" && adminToken == "" {
		log.Warn("admin API has no token (admin.tokenFile): anyone who can reach it can manage sessions and engines")
	}
	adm := admin.New(admin.Options{
		Sessions: sessions, Engines: engines, Metrics: met, Token: adminToken, Ready: ready,
		Live: func() bool { return true }, Log: log, Version: version,
	})

	// run starts the serving machinery; with HA it runs only while leading.
	run := func(ctx context.Context) error {
		if err := engines.Start(ctx); err != nil {
			return err
		}
		go sessions.Run(ctx)
		started.Store(true)
		log.Info("gateway ready", "listen", cfg.Server.Listen)
		<-ctx.Done()
		return nil
	}

	errc := make(chan error, 3)
	quackSrv := &http.Server{
		Addr: cfg.Server.Listen, Handler: gw,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration,
		IdleTimeout:       cfg.Server.IdleTimeout.Duration,
		ErrorLog:          serverErrorLog(log),
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}
	go func() { errc <- serve(quackSrv, cfg.Server.TLS, "quack", log) }()
	var adminSrv *http.Server
	if cfg.Admin.Listen != "" {
		adminSrv = &http.Server{Addr: cfg.Admin.Listen, Handler: adm, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- serve(adminSrv, cfg.Admin.TLS, "admin", log) }()
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if cfg.HA.Enabled {
		identity := os.Getenv("POD_NAME")
		if identity == "" {
			identity, _ = os.Hostname()
		}
		go func() {
			err := ha.Run(runCtx, cfg.HA, kube, namespace, identity,
				func(lctx context.Context) {
					leading.Store(true)
					met.IsLeader.Set(1)
					if err := run(lctx); err != nil {
						errc <- err
					}
					// The leader context ended (lease lost or released): stop
					// reporting ready at once, not only once client-go has tried
					// to release the lease, which can take another renewDeadline.
					leading.Store(false)
					met.IsLeader.Set(0)
				},
				func() {
					leading.Store(false)
					met.IsLeader.Set(0)
					// Losing the lease while serving: exit so that the pod
					// restarts as a clean follower.
					errc <- errors.New("lost leadership")
				}, log)
			if err != nil && !errors.Is(err, context.Canceled) {
				errc <- err
			}
		}()
	} else {
		met.IsLeader.Set(1)
		go func() {
			if err := run(runCtx); err != nil {
				errc <- err
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-errc:
		if runErr != nil {
			log.Error("fatal", "err", runErr)
		}
	}

	// Graceful shutdown: stop accepting, let in-flight requests finish. Engines
	// keep running; sessions are adopted by the next gateway process.
	timeout := cfg.Server.ShutdownTimeout.Duration
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Shortly before the deadline, queries that still wait for their engine
	// are answered with an error: a request that ends unanswered when the
	// process exits can be sent again by libcurl, and the query would run
	// twice. Fetches and uploads in flight may be repeated safely.
	abort := time.AfterFunc(timeout-min(timeout/4, 5*time.Second), gw.AbortQueries)
	defer abort.Stop()
	_ = quackSrv.Shutdown(shutdownCtx)
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutdownCtx)
	}
	cancelRun()
	sessions.Shutdown()
	if errors.Is(runErr, http.ErrServerClosed) {
		runErr = nil
	}
	return runErr
}

// serverErrorLog is the error log of the Quack listener. Most of its lines
// (TLS handshake errors of scanners, closed connections) are noise, but a
// recovered handler panic means a request went unanswered: that is an error.
func serverErrorLog(log *slog.Logger) *stdlog.Logger {
	return stdlog.New(errorLogWriter{log: log}, "", 0)
}

type errorLogWriter struct{ log *slog.Logger }

func (w errorLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if strings.Contains(msg, "panic serving") {
		w.log.Error("request handler panicked", "listener", "quack", "msg", msg)
	} else {
		w.log.Debug(msg, "listener", "quack")
	}
	return len(p), nil
}

func serve(srv *http.Server, t *config.TLSConfig, name string, log *slog.Logger) error {
	log.Info("listening", "listener", name, "addr", srv.Addr, "tls", t != nil)
	var err error
	if t != nil {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		err = srv.ListenAndServeTLS(t.CertFile, t.KeyFile)
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("%s listener: %w", name, err)
}
