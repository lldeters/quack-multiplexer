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

// labelCloseTimeout bounds the removal of the leader label at shutdown,
// before the lease is released (terminationGracePeriodSeconds covers it).
const labelCloseTimeout = 2 * time.Second

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
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if err := validateStartup(cfg); err != nil {
		return err
	}
	for _, w := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	fmt.Println("configuration is valid")
	return nil
}

// validateStartup runs the checks that "serve" makes at start-up as far as
// they need no cluster, no engine and no secret files: the provider
// settings (for Kubernetes with every profile rendered once) and the leader
// election settings.
func validateStartup(cfg *config.Config) error {
	var errs []error
	switch cfg.Provider.Type {
	case "kubernetes":
		errs = append(errs, kubernetes.Validate(cfg))
	case "process":
		// New only checks the settings; it starts nothing.
		_, err := process.New(cfg, slog.New(slog.DiscardHandler))
		errs = append(errs, err)
	case "static":
		errs = append(errs, static.Validate(cfg))
	}
	if cfg.HA.Enabled {
		errs = append(errs, ha.Validate(cfg.HA))
	}
	return errors.Join(errs...)
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
	for _, w := range cfg.Warnings() {
		log.Warn(w)
	}

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
		engineClient, rc, err := kubernetes.NewClient(cfg.Provider.Kubernetes)
		if err != nil {
			return fmt.Errorf("kubernetes client: %w", err)
		}
		// Leader election gets a client with a rate limiter of its own: a
		// burst of engine launches must not delay the renewals of its lease.
		if kube, err = kubernetes.NewClientForConfig(rc); err != nil {
			return fmt.Errorf("kubernetes client: %w", err)
		}
		kp, err := kubernetes.New(cfg, engineClient, log)
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

	var elector *ha.Elector
	var podNamespace, podName string
	if cfg.HA.Enabled {
		// The leader labels its own pod (see the labeler below), so the
		// gateway must run as a pod that knows its name and namespace; the
		// pod name is its identity in the lease.
		if podNamespace, podName, err = ha.OwnPod(); err != nil {
			return err
		}
		elector, err = ha.New(ha.Options{Config: cfg.HA, Client: kube, Namespace: namespace, Identity: podName, Log: log})
		if err != nil {
			return err
		}
		// A replica that may not use the lease or watch the engine pods
		// could never serve as the leader: refuse to start, so that a
		// rollout of it stalls while the old replicas keep serving.
		if err := elector.CheckAccess(ctx, namespace); err != nil {
			return err
		}
		// Background work that acts on engines runs only while this process
		// holds a fresh lease: a leader that resumes after a freeze must not
		// close sessions or stop engines that the new leader serves by now.
		sessions.SetFence(elector.Fresh)
		engines.SetFence(elector.Fresh)
	}

	var leading atomic.Bool
	var started atomic.Bool
	var stopping atomic.Bool
	// serving reports whether this process serves Quack requests, ready
	// whether its pod is ready (see readiness).
	rd := &readiness{stopping: &stopping, leading: &leading, started: &started, synced: engines.Synced}
	if elector != nil {
		rd.elector = elector
	}
	serving, ready := rd.serving, rd.ready
	var labeler *ha.Labeler
	if elector != nil {
		labeler, err = ha.NewLabeler(ha.LabelerOptions{
			Client: kube, Namespace: podNamespace, Pod: podName, Log: log, Metrics: met,
			Want: func() bool { ok, _ := serving(); return ok },
		})
		if err != nil {
			return err
		}
		// Remove a label that an earlier container of this pod left behind,
		// and refuse to start if the pod can never be labelled.
		if err := labeler.Init(ctx); err != nil {
			return err
		}
		rd.standby = func() (bool, string) { return ha.StandbyReady(elector, labeler) }
	}
	var fence func() bool
	if elector != nil {
		fence = elector.Fresh // the query time limit cancels queries on engines
	}
	gw := proxy.New(proxy.Options{Config: cfg, Auth: authn, Engines: engines, Sessions: sessions, Upstream: up, Log: log, Metrics: met,
		Ready: serving, Fence: fence})

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
		Sessions: sessions, Engines: engines, Volumes: engines, Metrics: met, Token: adminToken, Ready: ready,
		Live: func() bool { return true }, Log: log, Version: version,
	})

	// run starts the serving machinery; with HA it runs only while leading.
	run := func(ctx context.Context) error {
		if err := engines.Start(ctx); err != nil {
			return err
		}
		go sessions.Run(ctx)
		// The connections that the recovered engines hold as this process
		// takes over: the sweep probes those that no session adopts once
		// their engine lease has run out.
		sessions.SweepOrphans(ctx, engines.RecoveredTargets())
		started.Store(true)
		if labeler != nil {
			labeler.Kick() // label the pod now: from then on the Service routes to it
		}
		log.Info("gateway ready", "listen", cfg.Server.Listen)
		<-ctx.Done()
		return nil
	}

	// Room for every sender (both listeners, run, the elector and its
	// callbacks), so that none of them blocks once nobody reads any more.
	errc := make(chan error, 8)
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

	// The serving machinery and the leadership end when the shutdown below
	// says so, not with the signal (see there).
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	electorDone := make(chan struct{})
	if elector != nil {
		// The label outlives the leadership: it goes when serving ends, and
		// at shutdown before the lease is released.
		labelCtx, stopLabels := context.WithCancel(context.Background())
		defer stopLabels()
		go labeler.Run(labelCtx)
		go func() {
			defer close(electorDone)
			err := elector.Run(runCtx,
				func(lctx context.Context) {
					leading.Store(true)
					met.IsLeader.Set(1)
					if prev := elector.Displaced(); prev != "" {
						// The previous leader did not release the lease: its pod
						// may still carry the label, and on a lost node it stays
						// ready for a while.
						go func() { _ = labeler.RemoveStale(lctx, prev, elector.Fresh) }()
					}
					if err := run(lctx); err != nil {
						errc <- err
					}
					// The leader context ended (lease lost or released): stop
					// serving at once, not only once client-go has tried to
					// release the lease, which can take another renewDeadline.
					leading.Store(false)
					labeler.Kick()
					met.IsLeader.Set(0)
				},
				func() {
					leading.Store(false)
					labeler.Kick()
					met.IsLeader.Set(0)
					// Losing the lease while serving: exit so that the pod
					// restarts as a clean follower.
					errc <- errors.New("lost leadership")
				})
			if err != nil && !errors.Is(err, context.Canceled) {
				errc <- err
			}
		}()
	} else {
		close(electorDone)
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
	stopping.Store(true)
	if elector != nil {
		// Take the pod out of the Service at once (by its label, without
		// waiting for the readiness probe), hand the lease over and keep
		// answering until kube-proxy has caught up (see handover). The
		// background work ends with the leadership (and is fenced by its
		// freshness anyway).
		handover{
			keepAlives: quackSrv.SetKeepAlivesEnabled,
			unlabel:    labeler.Close, unlabelTimeout: labelCloseTimeout,
			release: cancelRun, released: electorDone, releaseTimeout: cfg.HA.RenewDeadline.Duration + time.Second,
			served: started.Load, delay: cfg.HA.DeregistrationDelay.Duration, log: log,
		}.run(shutdownCtx)
	}
	_ = quackSrv.Shutdown(shutdownCtx)
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutdownCtx)
	}
	// Without HA, the background work (engine events, session keep-alives)
	// stops only once the in-flight requests are done.
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
