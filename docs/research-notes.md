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
  `quack_authorization_function(connection_id, query)` can be SQL macros. The server runs
  `SELECT <setting>(?, ?)` (the setting's value is pasted into the SQL) on a fresh connection for every PREPARE,
  catalog queries included, with the query text as the client sent it. An error, NULL or false rejects the query
  ("Authorization failed", or "Authorization callback failed: <error>"); a VARCHAR result replaces the SQL (the
  default `quack_nop_authorization` returns the query itself). `src/quack_server.cpp`.
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
  covers the init SQL, `quack_serve` (with `.output /dev/null` around it), the post-start SQL and, on SIGTERM,
  `quack_stop`,
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
    `SET`s fail ("the configuration has been locked"). It blocks every `SET` and `RESET` except `schema` and
    `search_path` (and settings in `allowed_configs`), and `enable_logging()` and `enable_profiling()`
    (`DBConfig::CheckLock`). `SET VARIABLE` still works.
  - `SET enable_external_access = false` before `quack_serve` breaks the start: `getenv` is then disabled, and
    `quack_serve` itself needs file access (`$HOME/.duckdb/stored_secrets`). Set after `quack_serve`, it blocks
    `getenv` and file access for every session while the server keeps working (queries, uploads, fetches).
  - `quack_stop(…)` stays callable by any session, with or without a locked configuration.
  - Once `enable_external_access` is false it cannot be set to true again, and `allowed_paths` and
    `allowed_directories` cannot change any more. Switching it off at run time adds DuckDB's temp directory to
    `allowed_directories` and every attached database file (with `.wal`, `.wal.checkpoint` and `.wal.recovery`) to
    `allowed_paths` (`EnableExternalAccessSetting::OnSet`, `src/main/settings/custom_settings.cpp`). Every session
    can read and write them: `COPY … TO '<database file>' (USE_TMP_FILE false)` destroyed the database (the engine
    never started again), and `EXPORT DATABASE` into the temp directory, a `schema.sql` written with `COPY` and
    `IMPORT DATABASE` ran that SQL without the authorization function, which replaced the macro (`PragmaImportDatabase`
    in `src/function/pragma/pragma_queries.cpp` builds the SQL from the files). `SET temp_directory = ''` before it
    keeps the temp directory out (and spilling off). Other paths and URLs need `allowed_paths` or
    `allowed_directories` set before it; httpfs then also needs the CA bundle in `allowed_paths` ("Cannot read a CA
    certificate bundle"), unless an earlier HTTPS request read it. Databases attached before stay readable (their
    paths are allowed), views over `read_parquet(<url>)` do not. `INSTALL`, `LOAD` and autoload fail ("Loading
    external extensions is disabled through configuration"), and so does `duckdb_extensions()`, which lists the
    extension directory. `quack_stop` and `FORCE CHECKPOINT` on the engine's own connection still work.
  - Besides `query()` and `json_execute_serialized_sql`, SQL that is not in the text of a query runs through
    `PRAGMA import_database` and the external resources of DuckDB 2.0: `register_external_resource_type(…)` names
    callback functions by string, and `create_external_resource(…)` or `CREATE EXTERNAL RESOURCE` then run
    `SELECT * FROM <callback>(MAP {})` on an internal connection (`create_external_resource.cpp`). `CONNECT '<uri>'`
    attaches a database without the word `ATTACH` (`physical_connect.cpp`). `truncate_duckdb_logs()` has no lock
    check, `disable_logging()` has one.
  - The CLI hands its own state to every connection: `FROM _` (a replacement scan of the CLI) returns the last
    result of the CLI's connection, which after the start was the row of `quack_serve` with the token, and
    `shell_history()` returns the lines it read from stdin (not the contents of `.read` files). With file access,
    a session can write into the CLI's stdin through `/proc/self/fd/0` (`COPY … TO`); the CLI runs that on its
    own connection, dot commands such as `.shell` included. The entrypoint selects only `listen_uri` from
    `quack_serve`, ends with a harmless SELECT and makes the FIFO read-only (mode 0400) once fd 3 is open; the
    process provider does the same with its anonymous pipe. Root with `CAP_DAC_OVERRIDE` (the default capabilities
    of a container) opens it anyway.
  - A macro in `main` takes precedence over a built-in function of the same name for all connections, also over
    operators that are functions (`CREATE MACRO "||"(a, b)` changes `||`); `system.main.<name>` is not affected,
    but a schema named `system` makes it ambiguous. `CREATE TYPE` cannot shadow built-in types.
  - DuckDB 2.0 parses with a PEG tokenizer: identifiers have no escape sequences (no `U&"…"`), may be quoted with
    `"` or backticks, and Unicode spaces outside quotes become spaces (`Parser::NormalizeSQLString`). Function
    names and keywords match case-insensitively for ASCII letters only (a Kelvin sign for `k` does not resolve).
  - The authorization macro of the image (`images/engine/authorize.sql`) costs about 2 ms per query: the server
    binds it for every PREPARE and compiles every occurrence of its regular expression anew, about 1 ms each (2.1 ms
    without, 4.3 ms with it for a trivial query through `r.query`; 5.3 ms while the CASE held the expression twice).
  - What the init SQL creates stays visible to every session: paths of attached databases (`duckdb_databases()`,
    query strings included), view and macro definitions (`duckdb_views()`, `duckdb_functions()`), name, scope and
    `KEY_ID` of secrets.
- **Listing the connections of an engine** (for the orphan sweep, checked on 2026-11-25 against the engine image,
  source `src/quack_active_connections.cpp`, `src/quack_server.cpp`):
  - `quack_active_connections()` returns `server_id`, `connection_id`, `query`, `state`, `client_id_hash`
    (VARCHAR), `cached_rows` (BIGINT) and `query_started_at` (TIMESTAMP), one row per entry of the server's
    connection table: the connection that asks is listed as well, and so are connections whose lease ran out but
    that no request touched since (expiry is lazy). The authorization callback runs on a connection of its own
    that is not listed. Connection ids are 32 upper-case hex characters.
  - The scan writes all rows into one output vector: with more than 2048 connections it fails with
    `Vector::SetValue index 2048 is out of range for vector with capacity 2048` (Invalid Input, the engine stays
    usable); 2048 list fine.
  - The rows of a PREPARE come inline: the server drains whole result batches until `inline_rows` (from the
    request, else `quack_prepare_inline_rows`, 24576) is reached, so one batch may exceed it; `inline_rows = 0`
    sends none and sets `needs_more_fetch`. Chunks have up to 2048 rows; chunks below 1024 rows are merged into
    flat ones (`QuackChunkStager`), larger ones keep their vector form (a constant vector stays constant). The
    layout of the inline DataChunks (storage V2_0_0) is in `internal/wire/result.go`; recordings are in
    `internal/wire/testdata/duckdb-v2.0.0`.
  - Every message for a connection runs `RenewConnectionLease` before its handler. For a connection whose lease
    ran out, a HEARTBEAT (like any other message) answers `Connection heartbeat lease expired`, removes it and
    interrupts a query that runs on it (`CleanupExpiredConnection`: checked with a CPU-bound query on a 4-second
    lease, its PREPARE answered `connection heartbeat lease expired` (INTERRUPT) 8 ms after the HEARTBEAT, CPU
    97 % to 0 %); the next message answers `Invalid connection id`. For a valid lease the HEARTBEAT only renews it.
    So a probe with HEARTBEAT removes exactly the connections that nobody can use any more.
  - DISCONNECT of a connection with a valid lease removes it, but does not stop a query that runs on it (the
    handler keeps the connection until the query ends); CANCEL with the wildcard uuid first interrupts it at once.
  - A DuckDB client heartbeats `timeout/3 · U(0.8, 1.2)` after the previous heartbeat returned
    (`QuackPeriodicWorker`), so a live client sends at least one message within `0.4 × timeout` of the gateway
    becoming reachable, unless its previous heartbeat hangs. It does hang: the client keeps one keep-alive HTTP
    connection, a heartbeat waits up to `HTTP_TIMEOUT_SECONDS` (86400 s) without retries, and when the gateway's
    node vanishes without FIN or RST, it waits until TCP gives up (`tcp_retries2`, about 15 min by default), on a
    frozen gateway until that is killed. A review experiment (gateway A lost its network and was killed, B took
    over under the same name, `tcp_retries2=8`) saw the first message of the client at B 107 s later.
  - In a SQL macro, `=` does not resolve through the catalog (a macro named `=` changes nothing), but
    `default_collation = 'nocase'` makes `sql = '…'` case-insensitive; `sql = ('…' COLLATE c)` compares bytes
    under any default collation. `COLLATE` on a bare macro parameter fails at `CREATE MACRO` ("collations are
    only supported for type varchar"). `FROM r.query('<text>')` of a client sends `<text>` verbatim.
- **Kubernetes.** From Kubernetes 1.35 on the kubelet refuses hosts with cgroup v1 (`failCgroupV1: true`); kind needs
  a KubeletConfiguration patch there (`test/e2e/README.md`).
