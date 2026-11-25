# Recorded Quack traffic (DuckDB v2.0.0)

Real messages between a DuckDB client and a DuckDB Quack server, used by
`realtraffic_test.go`. Each file is one HTTP body of `POST /quack`.

| | |
|---|---|
| DuckDB | v2.0.0 (Cyanoptera) `d54cbbe3df`, official `duckdb-cli-linux-amd64.tar.gz` |
| quack extension | `f96420ed46` (the commit DuckDB v2.0.0 pins in `.github/config/extensions/quack.cmake`) |
| server | the engine image `images/engine` (`quack_serve` over plain HTTP) |
| client | the same CLI, `ATTACH 'quack:127.0.0.1:…' AS remote (HEARTBEAT_TIMEOUT 3)` |
| recorded | 2026-11-20 with a buffering reverse proxy between client and server |

The token in the handshakes is the test value `capture-token-0123456789`.
Connection ids, client ids and query uuids are random values of that run.

Files named `*.truncated.bin` hold the first 4096 bytes of a large message
(the test checks only their header). The other files are complete.

| File | Message |
|---|---|
| `connection-request.bin`, `connection-response.bin` | handshake (heartbeat timeout 3 s) |
| `connection-request-wrong-token.bin`, `error-authentication-failed.bin` | handshake with a wrong token |
| `prepare-catalog-*.bin` | catalog query of `ATTACH` (`query_uuid {0,0}`) |
| `prepare-begin-*.bin`, `prepare-commit-request.bin` | transaction control around a query; `COMMIT` carries no `client_query_id` |
| `prepare-query-*.bin` | a query with an inline result |
| `prepare-insert-*.bin` | `INSERT … scan_data_from_quack_client(…)` with `inline_rows = 0` |
| `prepare-inline-rows-response.truncated.bin` | `SELECT * FROM t`: 2.6 MB of inline rows |
| `send-data-request.bin`, `send-data-large-request.truncated.bin`, `send-data-response.bin` | upload of client rows (`CREATE TABLE remote.x AS SELECT … FROM local`) |
| `fetch-request.bin`, `fetch-response.bin` | fetch of the `INSERT` result |
| `fetch-read-ahead-request.bin`, `fetch-no-batch-response.bin` | one of 16 parallel read-ahead fetches and the answer for a batch that does not exist |
| `fetch-data-response.truncated.bin` | a 1.7 MB result batch |
| `acknowledgement-*.bin` | acknowledgement after a fetched scan (`SET quack_enable_reconnects = true`) |
| `cancel-*.bin` | the client cancels a query that failed |
| `prepare-error-request.bin`, `error-catalog.bin`, `error-invalid-input.bin` | query errors (unknown table, `error('boom')`) |
| `heartbeat-*.bin`, `disconnect-*.bin` | heartbeat and `DETACH` |
| `error-invalid-connection-id.bin` | heartbeat with an unknown connection id |
| `error-lease-expired.bin` | heartbeat after the client was killed and its 2 s lease ran out |

The query results below were recorded on 2026-11-25 from the same engine
image with a raw Quack client built on this package (`result_test.go`). Each
file is a complete answer to a PREPARE_REQUEST that asked for up to 65536
inline rows, unless noted.

| File | Query |
|---|---|
| `prepare-active-connections-response.bin` | `SELECT connection_id FROM quack_active_connections()` with `query_uuid {0,0}` from a control connection, with two other connections open: three rows, the asking connection among them |
| `error-active-connections-limit.bin` | the same query with 2049 connections open: `quack_active_connections()` writes every row into one vector of 2048 |
| `prepare-varchar-nulls-response.bin` | `SELECT CASE WHEN range % 3 = 0 THEN NULL ELSE 'v' \|\| range END AS v FROM range(7)` (a validity mask) |
| `prepare-varchar-constant-response.bin` | `SELECT 'quack' AS c FROM range(1500)` (one chunk with a constant vector) |
| `prepare-varchar-chunks-response.bin` | `SELECT 'row ' \|\| range AS v FROM range(3000)` (two chunks, 2048 and 952 rows) |
| `prepare-varchar-fetch-response.bin` | `SELECT range::VARCHAR AS v FROM range(30000)` with `inline_rows = 0` (no rows inline, `needs_more_fetch`) |
| `prepare-integer-response.bin` | `SELECT 42 AS n` (an INTEGER column) |

To check a whole recording, point `QUACK_CAPTURE_DIR` at a directory with
`NNNN-req.bin`, `NNNN-resp.bin` and `NNNN.meta` files (first line
`POST /quack`) and run `go test ./internal/wire/ -run RealTraffic`.
