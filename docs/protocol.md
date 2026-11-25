# Quack-Protokoll: was das Gateway davon wissen muss

Grundlage ist der Quellcode von [duckdb/duckdb-quack](https://github.com/duckdb/duckdb-quack), Stand November 2026
(Protokollversion `QUACK_VERSION = 3`, DuckDB 2.0.0). Relevante Dateien:

| Datei | Inhalt |
|---|---|
| `src/include/quack_message.json`, `src/serialize_quack_message.cpp` | Feldnummern und Serialisierung aller Nachrichten |
| `src/quack_http_server.cpp` | HTTP-Endpunkte (`POST /quack`, `GET /`, `OPTIONS /quack`) |
| `src/quack_server.cpp` | Verbindungs-, Lease-, Query- und Fetch-Logik des Servers |
| `src/quack_client.cpp` | Client: Header, Heartbeats, Verbindungs-Pool, Fehlerbehandlung |

## Transport

* `POST /quack`. Request-Body roh, Antwort mit `Content-Type: application/vnd.duckdb` und **immer HTTP 200**.
  Fehler sind `ERROR_RESPONSE`-Nachrichten. Andere Statuscodes meldet der Client nur als
  „Failed to send message: HTTP …“.
* `GET /` liefert ein Text-Banner ohne Authentifizierung. Es eignet sich als Readiness-Probe.
* `OPTIONS /quack` liefert die CORS-Freigabe für DuckDB-Wasm.
* Der Client hält pro Verbindung mehrere Keep-Alive-HTTP-Verbindungen (Pool pro asynchronem Thread) und
  schickt parallele Requests mit derselben `connection_id`, etwa Fetch-Ahead oder Heartbeats. Der Server
  ordnet ausschließlich über die `connection_id` zu, **nicht** über die TCP-Verbindung.
* Client-Timeout pro Request: 24 h (`HTTP_TIMEOUT_SECONDS`). Retries: 0.
* Server (cpp-httplib):
  * 5 s Socket-Read/Write-Timeout. Ein ins Stocken geratener Peer wird abgebrochen.
  * Request-Bodies bis 100 MiB.
  * Ein Thread pro TCP-Verbindung (`quack_server_max_connections`, 1024).
  * Keep-Alive 300 s (`quack_server_keep_alive_timeout`).
* `EXTRA_HTTP_HEADERS` aus dem `quack`-Secret hängt der Client an **jeden** Request, aber nur im
  httpfs-Transport. Mit `ssl_fingerprint` (Pinning) nutzt der Client einen eigenen httplib-Client ohne
  Zusatz-Header.

## Serialisierung (DuckDB `BinarySerializer`)

Ein Body besteht aus `Header-Objekt`, `Body-Objekt` und optional einem Roh-Blob mit DataChunks. Der Blob kommt bei
`FETCH_RESPONSE` und `SEND_DATA_REQUEST` vor.

* Objekt = Folge von Properties, abgeschlossen durch die Feld-ID `0xFFFF`.
* Property = Feld-ID (`uint16`, little endian) + Wert.
* Werte:
  * unsigned: ULEB128
  * signed: SLEB128
  * `bool`: 1 Byte
  * `string`: ULEB128-Länge + Bytes
  * `hugeint_t`: SLEB128(upper) + ULEB128(lower)
  * `optional_idx`: ULEB128, wobei `2^64-1` „nicht gesetzt“ bedeutet
  * `unordered_map<string,string>`: ULEB128-Anzahl, dann Objekte `{0: key, 1: value}`
  * Enum `MessageType`: ULEB128 des `uint8`
* `WriteProperty` schreibt immer. `WritePropertyWithDefault` **lässt die Property weg**, wenn sie den Defaultwert
  hat (leerer String, 0, `false`, leerer Vektor, ungesetztes `optional_idx`). Der Decoder schaut deshalb die nächste
  Feld-ID an, bevor er liest.

### Header (jede Nachricht)

| ID | Feld | Typ | Schreibweise |
|---|---|---|---|
| 1 | `type` | MessageType | immer |
| 2 | `connection_id` | string | entfällt, wenn leer |
| 3 | `client_query_id` | optional_idx | immer (`ff…ff01`, wenn ungesetzt) |

Beispiel `CONNECTION_REQUEST`-Header: `01 00 01 03 00 ff ff ff ff ff ff ff ff ff 01 ff ff`.

### Nachrichtentypen

| Wert | Typ | Richtung | Body-Felder |
|---|---|---|---|
| 1 | CONNECTION_REQUEST | C→S | 1 auth_string, 2 client_duckdb_version, 3 client_platform, 4 min_supported_quack_version, 5 max_supported_quack_version, 6 client_id, 7 heartbeat_timeout_seconds (alle „with default“) |
| 2 | CONNECTION_RESPONSE | S→C | 1 server_duckdb_version, 2 server_platform, 3 quack_version, 4 heartbeat_timeout_seconds; **connection_id im Header** |
| 3 | PREPARE_REQUEST | C→S | 1 sql_query, 2 query_uuid (hugeint, immer), 3 inline_rows (optional_idx, entfällt wenn ungesetzt) |
| 4 | PREPARE_RESPONSE | S→C | 1 result_types, 2 result_names, 3 needs_more_fetch, 4 results (inline DataChunks), 5 query_uuid |
| 7 | FETCH_REQUEST | C→S | 1 query_uuid, 2 batch_index, 3 ack_index |
| 8 | FETCH_RESPONSE | S→C | 1 chunk_count, 2 total_batches, 3 batch_index, danach Roh-Blob |
| 9 | SEND_DATA_REQUEST | C→S | 1 stream_id, 2 chunk_count, 3 total_batches, 4 batch_index, danach Roh-Blob |
| 10 | SUCCESS_RESPONSE | S→C | – |
| 11 | DISCONNECT_MESSAGE | C→S | – |
| 12 | CANCEL_REQUEST | C→S | 1 query_uuid (`{0,0}` = aktuell laufende Query) |
| 14 | SEND_DATA_RESPONSE | S→C | 1 accept_budget (optional_idx, immer) |
| 15 | ACKNOWLEDGEMENT | C→S | 1 query_uuid (entfällt bei `{0,0}`) |
| 16 | HEARTBEAT_REQUEST | C→S | – |
| 100 | ERROR_RESPONSE | S→C | 1 message, 2 exception_type, 3 extra_info, 4 must_invalidate |

## Semantik, die das Routing bestimmt

* **Handshake:** Der Server ruft `quack_authentication_function(connection_id, auth_string, server_token)` auf.
  Standard ist `quack_check_token`, also ein Token-Vergleich. Ein eigenes SQL-Makro ist möglich. Danach vergibt er
  eine zufällige `connection_id` (32 Hex-Zeichen) und legt einen eigenen DuckDB-`Connection`-Kontext an.
  Transaktionen, temporäre Objekte, `SET`s, laufende Query, Result-Cache und Insert-Streams hängen an dieser
  Verbindung.
* **Version:** Der Server lehnt ab, wenn `[min, max]` des Clients die 3 nicht enthält. Der Client verlangt in der
  Antwort exakt `quack_version == 3` und einen Heartbeat-Timeout in `[1, INT64_MAX/1000]`.
* **Lease:** Jede strukturell gültige Nachricht einer bestehenden Verbindung erneuert die Lease. Ist sie
  abgelaufen, antwortet der Server mit `"Connection heartbeat lease expired"` und entfernt die Verbindung.
  **Geprüft wird nur beim nächsten Request dieser Verbindung.** Einen Hintergrund-Sweeper gibt es nicht. Stirbt
  ein Client, bleibt seine Verbindung samt Kontext auf der Engine liegen. Der Client schickt Heartbeats etwa alle
  `timeout/3 · [0,8; 1,2]`.
* **Unbekannte Verbindung:** `"Invalid connection id"`. Ein `DISCONNECT` auf eine bereits geschlossene Verbindung
  ergibt `"Connection does not exist / already disconnected"`.
* **must_invalidate:** Der Server setzt das Flag, wenn seine eigene Datenbank invalidiert ist. Der Client markiert
  daraufhin das attachte Datenbankobjekt als tot, bis zu `DETACH` + `ATTACH`. Fehlertypen wie `FATAL` oder
  `INTERNAL` stuft der Client herab, damit seine lokale Datenbank nicht mit invalidiert wird.
* **FETCH:** Blockiert serverseitig, bis der angeforderte Batch fertig ist. Wiederholt der Client einen Request
  (gleicher `batch_index`), bekommt er denselben Batch; bestätigte Batches (`ack_index`) gibt der Server frei.
  Batches sind standardmäßig bis zu 32 MiB groß (`quack_target_batch_bytes`).
* **PREPARE** ersetzt die vorherige Query der Verbindung (Abbruch + neuer Treiber-Thread), auch eines mit
  `query_uuid {0,0}`, aber erst, nachdem `quack_authorization_function` es zugelassen hat. Die ersetzte Query
  bekommt `INTERRUPT` „superseded by a new query“, ihre späteren FETCHes „Result has been closed“. Bis zu 24 576
  Zeilen kommen inline in der Antwort (`quack_prepare_inline_rows`); das PREPARE antwortet erst, wenn diese Zeilen
  oder das Ende des Ergebnisses da sind.
* **Ende eines Ergebnisses:** `PREPARE_RESPONSE` mit `needs_more_fetch = false` (das Feld steht hinter den
  Spaltentypen) oder die abschließende `FETCH_RESPONSE` für einen Batch hinter dem letzten: ohne `chunk_count`, nur mit
  `total_batches`. Der Client holt mit parallelen Read-Aheads (16 bei 4 Kernen) und beendet einen Scan erst, wenn alle
  beantwortet sind. Bricht er einen Scan ab, schickt er `CANCEL` mit der `query_uuid` (nicht bei Katalog-Scans).
* **CANCEL:** `query_uuid {0,0}` bricht ab, was gerade läuft; eine andere uuid nur die Query, die gerade die Anweisung
  der Verbindung ist, sonst „Attempted to cancel a different query with id …“. Wartende PREPAREs und FETCHes der
  Query bekommen `INTERRUPT` „Interrupted: query was cancelled“, spätere FETCHes derselben uuid ebenso; Uploads
  scheitern, weil die Engine die Datenströme der Query schließt.
  Ein CANCEL auf eine ruhende Verbindung schadet nicht, auch nicht in einer offenen Transaktion; eine abgebrochene
  laufende Query bricht die Transaktion ab (gegen DuckDB v2.0.0 geprüft).
* **Katalog-Traffic** beim `ATTACH` nutzt `query_uuid = {0,0}`.

## Was quackmux daraus macht

| Aspekt | Umsetzung |
|---|---|
| Routing | Die versiegelte Gateway-Connection-ID enthält Engine-UID und Backend-ID. Sie ist höchstens ~140 Zeichen lang, wegen der 156-Byte-Grenze im SEND_DATA-Headerpuffer des Clients. Es gibt kein Routing über TCP-Affinität. |
| Heartbeat/DISCONNECT | Das Gateway beantwortet beide lokal. Die Engine-Lease (1 h) hält es selbst per Keepalive aktiv. |
| Header-Rewrite | Nur das Header-Objekt wird neu geschrieben. Body und Blob gehen byte-genau weiter. |
| Handshake | Wird vollständig dekodiert. `auth_string` wird durch das Engine-Token ersetzt, der Heartbeat-Timeout geklemmt. Das Gateway lehnt unbekannte Felder ab. |
| Antworten | Nur `CONNECTION_RESPONSE`-Header werden umgeschrieben. Alle anderen Antworten haben eine leere `connection_id`. |
| Fehlertexte | „Invalid connection id“ und „Authentication failed“ sind identisch zum Server. Eigene Fehler beginnen mit `quackmux:`. |
| Lease-Pflege | Das Gateway beobachtet `lastSeen` und schickt bei Ablauf ein `DISCONNECT`, damit die Engine aufräumt. |
| Verbindungsabbrüche | Ein Request, der schon gelesen wurde, bekommt immer eine Antwort, notfalls einen Quack-Fehler. libcurl könnte den POST sonst wiederholen und eine Query doppelt ausführen. |
| Puffern | Nachrichten bis 100 MiB (Request) bzw. 64 MiB (Response) werden vollständig gepuffert. Das Budget liegt bei 1 GiB. Ein langsamer Client bringt damit nie den 5-s-Timeout der Engine zum Auslösen. |
| Query-Zeitlimit | Optional (`limits.queryTimeout`). Das Gateway liest die `query_uuid` aus PREPARE, FETCH und CANCEL, den Anfang des SQL-Texts eines PREPARE (`COMMIT` und `ROLLBACK` haben kein Limit) und den Kopf der `FETCH_RESPONSE`. Nach Ablauf schickt es `CANCEL` mit der `query_uuid`, schneidet ein PREPARE ab, das die Engine noch nicht ganz hat, und beantwortet die Requests der Query mit einem eigenen `ERROR_RESPONSE` (siehe [Design, 5.3](design.md#53-query-zeitlimit)). |
