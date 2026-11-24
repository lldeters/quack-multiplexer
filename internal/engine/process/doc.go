// Package process is an engine provider for local development: every engine
// is a DuckDB CLI process on the gateway's machine, no Kubernetes needed.
//
// # Starting an engine
//
// Launch picks a free TCP port from provider.process.portRangeStart..End
// (probing it with net.Listen on bindHost and releasing it again), creates
// the scratch directory <workDir>/<engine id> and runs
//
//	<duckdbPath> -bail <database>
//
// in it, with the child's standard input connected to a pipe that the
// provider keeps open for the whole lifetime of the engine. On Linux the
// pipe is read-only (mode 0400): the CLI runs whatever it reads there on its
// own connection, dot commands such as .shell included, and a session with
// file access could otherwise open it again for writing through
// /proc/self/fd/0. All SQL travels through that pipe and never through the
// command line or the environment, where other local users or processes
// could read it (the profile's init and post-start SQL may carry
// credentials):
//
//	<engine.RenderInitSQL(profile)>
//	.output NUL                      (/dev/null outside Windows)
//	SELECT listen_uri FROM quack_serve('quack:<bindHost>:<port>',
//	    token = getenv('QUACK_TOKEN'), disable_ssl = true,
//	    allow_other_hostname = <bool>);
//	.output stdout
//	<engine.RenderPostStartSQL(profile)>   (only with post-start SQL:)
//	.output NUL
//	SELECT 'started' AS quackmux_engine;
//	.output stdout
//
// The token reaches the CLI through its environment (QUACK_TOKEN, see
// below), not through the pipe: every connection of the engine can read the
// lines that the CLI read from its standard input (shell_history()), and
// with them the init and post-start SQL. For the same reason only the
// address of the row that quack_serve returns is selected, with the output
// going nowhere: every connection can read the CLI's last result (FROM _),
// which the final SELECT also replaces after the post-start SQL.
// allow_other_hostname is false only for localhost, 127.0.0.1 and ::1, the
// addresses quack_serve accepts by default. quack_serve returns at once and
// serves from a background thread, while the CLI runs the post-start SQL
// and waits for more input. -bail makes the CLI exit on the first failing
// statement, so a broken init or post-start SQL or an unavailable port ends
// the process instead of leaving a half-initialized engine behind. Like
// every DuckDB CLI started without -init, the engine also runs the user's
// ~/.duckdbrc first.
//
// # Trust
//
// An engine runs as the gateway's operating system user. Without hardening
// (the authorization macro of images/engine and the post-start settings,
// deploy/kubernetes/README.md) DuckDB's file access lets every session read
// and write whatever that user may: the gateway's configuration and
// secrets, the environment of the other engines with their tokens, and
// ~/.duckdbrc, which the next engine runs (dot commands such as .shell
// included). So a session can run commands as that user. Without hardening,
// serve only trusted clients with this provider.
//
// A relative database path is resolved against the scratch directory and is
// therefore deleted together with the engine; use an absolute path for data
// that must survive it (a database file can only be opened by one engine
// process at a time). The defaults of DuckDB itself are left alone, including
// extension auto-installation: the quack extension must be installed or
// installable on the machine.
//
// # Environment
//
// The child inherits the gateway's environment except QUACK_TOKEN and every
// QUACKMUX_* variable: an engine executes whatever SQL its users send, and
// getenv() would otherwise reveal gateway secrets such as
// QUACKMUX_SESSION_KEY. Plain-value entries of the profile's env follow
// (entries with valueFrom cannot be resolved outside Kubernetes and are
// skipped with a warning), then the engine container contract of package
// engine: QUACK_TOKEN, QUACKMUX_DATABASE, QUACKMUX_PORT, QUACKMUX_ENGINE_ID,
// QUACKMUX_SHARE_LEVEL, QUACKMUX_OWNER, QUACKMUX_SUBDOMAIN and
// QUACKMUX_PROFILE. QUACKMUX_INIT_SQL and QUACKMUX_POST_START_SQL are
// deliberately not set: the SQL goes through standard input.
//
// # Readiness and failure
//
// Every 200ms the provider sends GET / to the engine, which the Quack server
// answers with its banner and HTTP 200 without authentication. The first
// success reports PhaseReady with the endpoint, possibly while the
// post-start SQL still runs. If the process exits first, or later without
// being asked to, the provider reports PhaseFailed with the exit status and
// the last 2 KiB of the engine's combined stdout and stderr (the token
// redacted). A failed engine keeps its scratch directory for inspection
// until the manager terminates it (engine.failedRetention).
//
// # Termination
//
// Terminate closes the engine's standard input; the CLI exits on end of
// input, which stops the Quack server. A process that is still running after
// the stop timeout (5s) is killed: on Unix its whole process group, on other
// systems the process itself (so duckdbPath should name the DuckDB binary,
// not a wrapper script). Then the scratch directory is removed and
// PhaseGone is reported. Run terminates all engines when its context ends.
//
// # Limitations
//
// Engines never survive the gateway, and the provider does not recover
// anything across gateway restarts: List only returns engines started by this
// provider instance. Even a crashed gateway leaves no engines behind, because
// the operating system closes the input pipes and every CLI exits on end of
// input. The scratch directories of engines that were not terminated
// properly remain under workDir (by default in the system's temporary
// directory). engine.Manager does not wait for Run to return when the gateway
// shuts down; callers that want the scratch directories removed can wait for
// Done.
package process
