# Research notes (verified facts behind the design)

Condensed results of the source and docs research done on 2026-11-19 against duckdb-quack `main`
(commit `02c9d4a`, 2026-11-17, `QUACK_VERSION = 3`), DuckDB core `181615215e5e` (2.0.0) and the
DuckDB 2.0 docs, checked on 2026-11-20 against the released DuckDB v2.0.0 (see the last section).
Everything quackmux relies on is listed here. File paths refer to
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
  pinned path (`PinnedHttpsQuackClient`) sends none. Routing options travel in the token (`secret?share_level=…`)
  or in those headers, not as ATTACH options: quack `main` rejects unknown ATTACH options (`quack_storage.cpp`),
  the version shipped with DuckDB 2.0.0 ignores them silently.
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

## Verified against the released DuckDB v2.0.0 (2026-11-20)

Done with the official `duckdb-cli-linux-amd64.tar.gz` (`v2.0.0 (Cyanoptera) d54cbbe3df`), the engine image,
docker compose and kind (Kubernetes v1.37). Recordings of the traffic are fixtures in
`internal/wire/testdata/duckdb-v2.0.0`.

- **Shipped quack version.** DuckDB v2.0.0 pins duckdb-quack `f96420ed46` (`.github/config/extensions/quack.cmake`),
  an ancestor of the `02c9d4a` researched above. The message definitions (`quack_message.json`,
  `serialize_quack_message.cpp`) are identical; the differences are request logging, internal API updates and two
  ATTACH checks `f96420e` does not have yet (unknown options, `READ_ONLY`).
- **Wire format.** 400 recorded messages (client ↔ server, client ↔ gateway, gateway ↔ engine) decode and encode
  byte for byte with `internal/wire`. Server answers never carry a `client_query_id`; only `CONNECTION_RESPONSE`
  carries a connection id (32 upper-case hex characters). The server leaves out an empty `extra_info` and a false
  `must_invalidate`.
- **Error texts** (all with exception type `Invalid Input`): `Authentication failed`, `Invalid connection id`,
  `Connection heartbeat lease expired`. After an expired lease the connection is gone: the next message gets
  `Invalid connection id`.
- **Client behaviour seen on the wire.**
  - DuckDB 2.0 pushes whole queries down, expressions and aggregates included
    (`SELECT ('q1=' || count_star()) FROM t`). `BEGIN TRANSACTION` and `COMMIT` are separate PREPAREs; `COMMIT`
    has no `client_query_id`.
  - A scan that fetches starts 16 parallel read-ahead FETCHes (4 cores); those for batches that do not exist get
    a short `FETCH_RESPONSE` (`total_batches` only).
  - ACKNOWLEDGEMENT is sent only with `SET quack_enable_reconnects = true`.
  - The ATTACH option `heartbeat_timeout` sets the proposed lease; the client accepts the larger value the gateway
    answers (3 s proposed, `session.minHeartbeatTimeout` 5 s answered).
  - Gateway connection ids observed: 88 to 90 characters (limit 156). A SEND_DATA of 7.6 MB passes the gateway
    with only the header rewritten.
- **Engine image.** `.read` under `-bail`, `getenv('QUACK_TOKEN')` in the CLI, the named `quack_serve` parameters
  (`token`, `allow_other_hostname`, `disable_ssl`) and the SIGTERM path (`quack_stop`, `FORCE CHECKPOINT`, `.quit`)
  work as the entrypoint expects; the image self-test passes. The `process` provider's scripts work as well.
- **Extensions.** `extension_directory` is deprecated in 2.0 and warns on every `SET`; `extension_directories`
  (`VARCHAR[]`) replaces it for INSTALL and LOAD. INSTALL downloads from `http://extensions.duckdb.org` (plain
  HTTP); the `http_proxy` setting defaults to the `HTTP_PROXY` environment variable. Extensions installed from a
  mirror (`custom_extension_repository`) load with a plain `LOAD`, because they are core-signed.
- **Hardening of shared engines.**
  - `SET lock_configuration = true` as the last init statement works: clients attach and query, only their own
    `SET`s fail ("the configuration has been locked").
  - `SET enable_external_access = false` before `quack_serve` breaks the start: `getenv` is then disabled, and
    `quack_serve` itself needs file access (`$HOME/.duckdb/stored_secrets`). Set after `quack_serve`, it blocks
    `getenv` and file access for every session while the server keeps working (queries, uploads, fetches).
  - `quack_stop(…)` stays callable by any session, with or without a locked configuration.
- **Kubernetes.** From Kubernetes 1.35 on the kubelet refuses hosts with cgroup v1 (`failCgroupV1: true`); kind needs
  a KubeletConfiguration patch there (`test/e2e/README.md`).

## Query lifecycle and CANCEL (checked against DuckDB v2.0.0, 2026-11-23)

The facts behind the query time limit (`internal/proxy/querylimit.go`). Source: `quack_server.cpp`
(`HandleMessageInternal`, `AbortStatement`), `quack_scan.cpp`, `quack_fetch_ahead.cpp`; probes with raw Quack
messages against the engine image and runs of the DuckDB 2.0 CLI through the gateway.

- **One statement per connection.** Every PREPARE replaces the connection's statement (`AbortStatement`, also with
  `query_uuid {0,0}`), but only after `quack_authorization_function` allowed it; a PREPARE that is denied or cannot be
  decoded leaves the running statement alone. The replaced PREPARE gets `INTERRUPT` "superseded by a new query",
  later FETCHes of its uuid "Result has been closed".
- **CANCEL.** `{0,0}` cancels whatever runs. Another uuid must be the uuid of the connection's statement, else the
  answer is "Attempted to cancel a different query with id '…' instead of '…'" and nothing happens. The statement keeps
  its uuid after it finished, so a CANCEL with the uuid of a finished query succeeds. A waiting PREPARE and every
  waiting FETCH get `INTERRUPT` "Interrupted: query was cancelled" at once, later FETCHes of the uuid the same error;
  the next PREPARE works. The upload streams of the statement are closed.
- **`quack_cancel(catalog, connection_id)`** (`quack_cancel.cpp`) is a client-side table function: it sends CANCEL
  with the zero uuid and the *target* connection id in the header over the catalog's connection. Through the gateway
  the target must be a gateway connection id like any other routed request.
- **Idle CANCEL is harmless.** A CANCEL (own uuid or `{0,0}`) on a connection whose query is over, also inside
  `BEGIN … COMMIT`, does not affect the next statement or the commit. Cancelling a running query inside a transaction
  aborts it ("Current transaction is aborted (please ROLLBACK)"); the transaction is rolled back.
- **End of a result.** PREPARE answers once `quack_prepare_inline_rows` rows or the end are there; `needs_more_fetch`
  (field 3) follows the result types (field 1, `vector<LogicalType>` with nested type infos and values), so a proxy
  needs a full LogicalType decoder to read it. A FETCH beyond the last batch gets `FETCH_RESPONSE` without
  `chunk_count` (body `02 00 <total> ff ff`); a data batch starts `01 00 <chunk_count> 03 00 <batch_index> ff ff`.
  FETCH with batch index 0 gets "FETCH_REQUEST is missing its batch index".
- **Client.** Each query has a random uuid. The fetcher sends read-ahead FETCHes (16 with 4 cores) and finishes a
  scan only when every one is answered, so late read-aheads do not overlap the next query. A scan that stops early
  sends CANCEL with its uuid (`~QuackScanBindData`), except catalog scans, whose bind data keeps uuid `{0,0}`.
- **Where long queries meet the limit.** DuckDB 2.0 often pushes an aggregate over `remote.query(…)` into the engine
  (`SELECT count(*) FROM remote.query(…)` was pushed, `SELECT 'x=' || CAST(count(*) AS VARCHAR) FROM remote.query(…)`
  was not: the client fetched every row). Results are cut into batches of up to 32 MiB (`quack_target_batch_bytes`),
  emitted in order; a selective filter over a huge range emitted nothing until its end. A long query computed on the
  engine therefore still waits in its PREPARE when the limit passes. When the client pulls the rows (a join with a
  local table, or an aggregate that is not pushed down), a FETCH gets the error; the client then needs seconds to
  wind down its read-aheads (5-15 s on a loaded 4-core machine with 32 MiB batches).
- **DISCONNECT with requests in flight** (checked in `quack_server.cpp` f96420e and against DuckDB 2.0, 2026-11-25).
  `DisconnectConnection` only erases the connection from the map of connections. Every handler holds the connection
  (`shared_ptr` in `DispatchMessage`) while it runs, and only `~QuackConnection`, when the last handler returns,
  aborts the statement: a PREPARE or a FETCH in flight runs on, and its statement with it, while a CANCEL after the
  DISCONNECT gets "Invalid connection id". A query with a 3 s limit whose client sent DISCONNECT on another request
  delivered its result after 14 s; without a request in flight the DISCONNECT ends the statement at once.
- **A request reaches the server whole.** The `/quack` handler (`quack_http_server.cpp`) reads the whole body into a
  `MemoryStream` before `HandleMessage`, which then serves the statement the connection has at that moment: a FETCH
  sent before the PREPARE of its uuid, with a body that arrives after that PREPARE was installed, long-polls a batch
  of that query.
- **Engine-side alternative.** `SET max_execution_time = <ms>` makes the engine end a query with `INTERRUPT` "Query
  exceeded maximum execution time"; a client can change it with `SET` unless the configuration is locked. Checked
  with DuckDB 2.0 on 2026-11-25: in the init SQL, a plain `SET max_execution_time = 2000` stays with the init
  connection (sessions see `0`, a count over 10^11 rows ran 8 s), while `SET GLOBAL max_execution_time = 2000;
  SET lock_configuration = true` holds for every session: they see `2000`, the count ends after 2.0 s, and their
  `SET` or `SET GLOBAL` of it fails ("the configuration has been locked"). It also ends a statement that no
  request waits for: a PREPARE with `inline_rows 0` answered at once, left alone, stopped the engine's work after
  2 s, and the next FETCH got the engine's error.
- **Statements that run on after their PREPARE was answered** (checked against DuckDB 2.0, 2026-11-25). The engine
  runs a statement on a thread of its own: with `inline_rows 0` it answers the PREPARE after planning, and of a SQL
  text with several statements the later ones run after the answer that carries the first one's rows (a
  `CREATE TABLE ... AS` after a `SELECT` ran about 15 s with a limit of 3 s and committed, when the client fetched
  nothing). The fetch stream ends only after the last statement: the stock client, which fetches its result, got
  the timeout error after 3 s for `remote.query('SELECT * FROM range(30000); CREATE TABLE ... AS ...')`, and the
  table was not created.
