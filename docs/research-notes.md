# Research notes (verified facts behind the design)

Condensed results of the source and docs research done on 2026-11-19 against duckdb-quack `main`
(commit `02c9d4a`, 2026-11-17, `QUACK_VERSION = 3`), DuckDB core `181615215e5e` (2.0.0) and the
DuckDB 2.0 docs. Everything quackmux relies on is listed here. File paths refer to
[duckdb/duckdb-quack](https://github.com/duckdb/duckdb-quack) unless stated otherwise.

## Quack client behaviour that constrains a proxy

- **Connection-id length is limited.** The client writes the SEND_DATA header into a fixed 256-byte buffer
  (`QUACK_PAYLOAD_HEADER_BYTES`). It throws `InternalException("SEND_DATA header does not fit its reserved space")`
  when `64 + len(connection_id) + len(stream_id = 36) > 256` (`src/quack_send_data.cpp:99`). That invalidates the
  client's whole DuckDB instance. **Gateway ids must be ≤ 156 bytes.** quackmux ids are about 90 and at most about
  140 characters.
- **Errors use HTTP 200.** Every failure must come back as `ERROR_RESPONSE`. Other statuses only show
  "IO Error: Failed to send message". The pinned-TLS path requires exactly 200.
- **No pipelining, but a lot of parallelism.** Per scan there are up to `quack_fetch_read_ahead` (default
  `async_threads` = 4 × cores, max 256) parallel long-polling FETCHes on separate HTTP connections. SEND_DATA
  (up to about 32 MiB each), HEARTBEAT, CANCEL and catalog PREPAREs (`query_uuid {0,0}`) run in parallel as well.
  Requests must be routed by the header `connection_id` only.
- **Timeouts.** Client requests time out after 86400 s (`HTTP_TIMEOUT_SECONDS`). Clients that pin
  `ssl_fingerprint` use `http_timeout`, 30 s by default, which matters for engine cold starts. There are no
  Quack-level retries. libcurl may resend a POST when a reused socket was closed before any answer, which would
  re-run a PREPARE. **Never close a connection after reading a request without answering it.**
- **Heartbeats** run at `timeout/3 · [0.8, 1.2]`, and failures are swallowed. A hung heartbeat blocks DETACH.
  DETACH sends DISCONNECT with a 2 s timeout.
- **Extra headers.** `EXTRA_HTTP_HEADERS` (quack secret) are sent on every request on the httpfs path only; the
  pinned path (`PinnedHttpsQuackClient`) sends none. ATTACH rejects unknown options, so routing options travel in
  the token (`secret?share_level=…`) or in those headers.
- **TLS default.** The client assumes HTTPS for every non-localhost host unless `DISABLE_SSL true`.
- **Content-Type** is not reliable: curl sends `application/octet-stream`. curl sends `Expect: 100-continue` for
  bodies over 1 MiB.

## Quack server behaviour

- `POST /quack` (binary), `GET /` (unauthenticated banner, usable as a probe), `OPTIONS /quack` (CORS).
- A connection is a server-side DuckDB `Connection` keyed by `connection_id` (32 upper-case hex characters),
  **not** by TCP connection.
- **Leases expire lazily**, only when a request with that id arrives (`RenewConnectionLease`). There is no
  sweeper, so abandoned connections live until the process dies. quackmux sends DISCONNECT itself.
- `quack_authentication_function(connection_id, auth_string, server_token)` and
  `quack_authorization_function(connection_id, query)` can be SQL macros. An authorization function that
  returns VARCHAR rewrites the SQL.
- cpp-httplib server limits:
  - 5 s socket read and write timeouts, so a stalled peer gets dropped;
  - bodies up to 100 MiB;
  - one thread per TCP connection (`quack_server_max_connections` 1024);
  - keep-alive 300 s (`quack_server_keep_alive_timeout`).

  Set these with `SET GLOBAL` **before** `quack_serve`.
- Result batches are up to 32 MiB (`quack_target_batch_bytes`). Up to 24 576 rows come inline in the PREPARE
  response.

## Running DuckDB 2.0 as a server in a container

- `quack_serve(uri, token, allow_other_hostname, disable_ssl, ssl_cert_file, ssl_key_file, secret,
  create_secret_if_not_exists)` binds and returns **immediately**. Its result row contains the token, so never
  print it.
- Binding `0.0.0.0` requires `allow_other_hostname = true`. A non-local address defaults to HTTPS with a
  self-signed certificate unless `disable_ssl = true`.
- **CLI behaviour:**
  - The CLI exits on stdin EOF, which stops the server. Kubernetes containers get EOF unless `stdin: true`.
  - `-serve` is not a daemon mode and cannot bind 0.0.0.0.
  - The CLI handles only SIGINT. As PID 1 it ignores SIGTERM, so run it under tini.
  - Order of execution: `-init` first, then `-cmd`, then stdin. `getenv()` works in the CLI.
- **Image:** `duckdb/duckdb:2.0.0` is distroless and has no extensions. Pre-install quack and httpfs into
  `extension_directory` and disable autoinstall at runtime. The release assets are
  `duckdb-cli-linux-{amd64,arm64}.tar.gz`.
- **quackmux's engine image** (`images/engine`) keeps a FIFO open on fd 3 and feeds statements through it. That
  covers the init SQL, `quack_serve` (with `.output /dev/null` around it) and, on SIGTERM, `quack_stop`,
  `CHECKPOINT` and `.quit`.

## Kyuubi semantics mirrored

- Share levels:
  - CONNECTION: one engine per session, stopped when the session closes.
  - USER, GROUP (primary group) and SERVER.
  - Subdomain, default `default`.
  - Engine pool (`pool.size`, `selectPolicy` RANDOM or POLLING).
- Timer defaults:
  - `kyuubi.session.idle.timeout` PT6H (sessions with no open operation);
  - `kyuubi.session.engine.idle.timeout` PT30M (engine without sessions);
  - `kyuubi.session.engine.initialize.timeout` PT3M.
- Kyuubi engines terminate themselves. In quackmux the gateway reaps engines, and Kubernetes labels replace
  ZooKeeper for discovery.
- Kyuubi loses sessions on a server crash. quackmux keeps them through sealed connection ids and adoption.

## Not yet verified against a real DuckDB 2.0 (no binary available while building)

- The engine entrypoint: `.read` under `-bail`, the exact `quack_serve` parameter spelling at runtime, `getenv`
  in a `-bail` session, and the SIGTERM path.
- The exact client error texts the e2e scripts grep for.
- Whether `lock_configuration = true` on shared engines breaks client internals.
