# quackmux – Design

quackmux ist ein Multi-Tenant-Gateway für das DuckDB-Quack-Protokoll, nach dem Vorbild von Apache Kyuubi.
DuckDB-Clients verbinden sich per `ATTACH 'quack:<gateway>'` mit dem Gateway. Das Gateway authentifiziert sie
und wählt anhand von Optionen (Share-Level, Subdomain, Profil) eine *Engine* aus. Eine Engine ist ein Pod mit
DuckDB 2.0 und `quack_serve`. Das Gateway startet Engines bei Bedarf und multiplext Sessions darauf. Es räumt
Sessions und Engines nach Timeouts wieder ab.

```
 DuckDB-Client ──HTTP(S) POST /quack──▶ quackmux ──HTTP POST /quack──▶ Engine-Pod (DuckDB + quack_serve)
 (ATTACH)                                 │  ▲                          Engine-Pod …
                                          ▼  │ Watch/Create/Delete
                                    Kubernetes-API (Pods, Secrets, Lease)
```

## 1. Protokoll-Grundlagen (Quack v3, DuckDB 2.0)

* Ein einziger Endpoint: `POST /quack`, Body = DuckDB-`BinarySerializer`-Objekte:
  `Header-Objekt | Body-Objekt | optionaler Roh-Blob` (Chunks bei `FETCH_RESPONSE`/`SEND_DATA_REQUEST`).
  Antworten kommen **immer mit HTTP 200**; Fehler sind `ERROR_RESPONSE`-Nachrichten. Bei Nicht-200 meldet der
  Client nur „Failed to send message“.
* Header: `{1: type (u8), 2: connection_id (string, entfällt wenn leer), 3: client_query_id (optional_idx)}`.
* Der Server vergibt die `connection_id` im Header der `CONNECTION_RESPONSE`. An ihr hängt serverseitig ein
  eigener DuckDB-`Connection`-Kontext: Transaktionen, temporäre Tabellen, `SET`-Variablen, laufende Query und
  Result-Cache. **Alle Requests einer Verbindung müssen deshalb dieselbe Engine erreichen.**
* Authentifizierung: `auth_string` im Body von `CONNECTION_REQUEST`. Der Server ruft
  `quack_authentication_function(connection_id, auth_string, server_token)` auf. Standard ist der Token-Vergleich.
* Heartbeat-Lease: Der Client schlägt `heartbeat_timeout_seconds` vor (Default 60 s) und sendet etwa alle
  `timeout/3` Sekunden einen `HEARTBEAT_REQUEST`. Jede gültige Nachricht verlängert die Lease. **Der Server prüft
  die Lease aber nur lazy**, also erst beim nächsten Request dieser Verbindung. Verbindungen abgestürzter Clients
  bleiben ohne fremdes Zutun ewig liegen. Das Gateway räumt sie aktiv per `DISCONNECT` ab.
* Optionen kann der Client nur über zwei Wege mitgeben:
  1. `EXTRA_HTTP_HEADERS` im `quack`-Secret. Sie gehen an jeden HTTP-Request, aber nur über den
     httpfs-Transport, nicht bei SSL-Fingerprint-Pinning.
  2. Den Token selbst. Er gelangt bei jedem Transport ins Gateway.

  Die URI kennt keinen Pfad (`quack:host:port`).

Details und Byte-Layouts: [protocol.md](protocol.md).

## 2. Begriffe

| Begriff | Bedeutung |
|---|---|
| **Session** | Eine Quack-Verbindung eines Clients (ein `ATTACH` bzw. ein `quack_query`-Aufruf). Entspricht einer Kyuubi-Session. |
| **Engine** | Ein DuckDB-Prozess mit Quack-Server, in Kubernetes ein Pod. Entspricht einer Kyuubi-Engine. |
| **Engine-Key** | `(ShareLevel, Owner, Subdomain, Profil, PoolIndex)`. Sessions mit gleichem Key teilen sich eine Engine. |
| **Profil** | Engine-Typ: Image, Ressourcen, Init-SQL, DuckDB-Settings, Pod-Template, Pool-Größe, Rechte. |
| **Backend-Connection-ID** | Die von der Engine vergebene `connection_id`. |
| **Gateway-Connection-ID** | Die ID, die der Client sieht: eine versiegelte Route (siehe 4.2). |

## 3. Share-Levels (wie Kyuubi)

| Share-Level | Owner im Engine-Key | Wirkung |
|---|---|---|
| `CONNECTION` | Session-ID | Jede Session bekommt einen eigenen Pod. Er wird beendet, sobald die Session endet (`engine.idleTimeout.CONNECTION = 0`). Optional gibt es einen Warm-Pool vorgestarteter Pods. |
| `USER` (Default) | Benutzername | Ein Pod pro Benutzer (und Subdomain/Profil), geteilt von allen Sessions dieses Benutzers. |
| `GROUP` | Gruppenname | Ein Pod pro Gruppe. Der Benutzer muss Mitglied sein. Option `group` wählt die Gruppe, Default ist die erste Gruppe. |
| `SERVER` | – | Ein Pod für alle (pro Subdomain/Profil). |

Die **Subdomain** (`[a-z0-9-]`, max. 32 Zeichen) erlaubt mehrere getrennte Engines je Owner, etwa
`subdomain=etl` und `subdomain=adhoc`. Bei `CONNECTION` ist sie bedeutungslos.

Ein **Pool** (`profile.pool.size = N`, `selectPolicy = RANDOM|POLLING`) verteilt Sessions gleichen Keys auf N
Engines, wie `kyuubi.engine.pool.*`. Vorsicht: Jede Engine hat ihren eigenen Datenbankzustand. Pools eignen sich
deshalb nur für zustandslose Workloads, zum Beispiel Lesen aus einem Data Lake oder DuckLake.

## 4. Request-Verarbeitung

### 4.1 Verbindungsaufbau (`CONNECTION_REQUEST`)

1. Body vollständig lesen (Limit 64 KiB) und strikt dekodieren. Unbekannte Felder führen zu einem Fehler.
   Der Client muss Protokollversion 3 unterstützen.
2. Token zerlegen: `<credential>[?option=wert&option=wert…]`.
3. Credential authentifizieren (`none` | `static` | `jwt`). Ergebnis ist die Identität `{User, Groups}`.
4. Optionen auflösen. Priorität: Token-Optionen > `X-Quackmux-*`-Header > passende `overrides[].defaults` >
   globale `defaults`. Danach Policy prüfen: Share-Level und Profil erlaubt, Gruppenmitgliedschaft, Limits pro
   Benutzer und global.
5. Engine-Key bilden. Pool-Index per Select-Policy wählen.
6. `engines.Acquire(key)`: Gibt es eine READY-Engine, wird sie genutzt. Startet gerade eine, wartet der Request.
   Sonst wird eine neue gestartet und auf READY gewartet (Startup-Timeout). Gleichzeitige Connects auf denselben
   Key teilen sich einen Start (Single-Flight). Der Start läuft weiter, auch wenn der wartende Client abbricht.
7. Den Request an die Engine weiterleiten. `auth_string` wird durch das Engine-Token ersetzt, der
   Heartbeat-Timeout auf `[session.minHeartbeatTimeout, session.maxHeartbeatTimeout]` geklemmt.
8. Antwortet die Engine mit `CONNECTION_RESPONSE`, wird geprüft, dass die Engine Version 3 spricht. Dann legt das
   Gateway die Session an und ersetzt die Backend-Connection-ID im Response-Header durch die
   Gateway-Connection-ID. Ein `ERROR_RESPONSE` geht unverändert an den Client. Bei `must_invalidate` gilt die
   Engine als FAILED.
9. Scheitert der Connect auf einer eigens gestarteten `CONNECTION`-Engine, wird diese sofort beendet.

### 4.2 Gateway-Connection-ID (versiegelte Route)

`qm` + base64url(nonce ‖ AES-256-GCM(binärer Payload) ‖ Tag). Der Nonce besteht aus 12 Zufallsbytes und ist
zugleich die Session-ID. Der Payload enthält:

* Version
* Engine-UID: 8 Bytes, abgeleitet aus der Pod-ID
* Backend-Connection-ID: 16 Bytes Hex-Rohdaten
* Erstellzeit
* Heartbeat-Timeout
* Benutzer: bis 40 Bytes, sonst ein Digest

**Längengrenze:** Der DuckDB-Client schreibt den SEND_DATA-Header in einen festen 256-Byte-Puffer und wirft eine
`InternalException`, wenn `64 + len(connection_id) + 36 > 256`. Die Exception invalidiert die *gesamte*
Client-Datenbank. Gateway-IDs dürfen deshalb höchstens 156 Bytes lang sein. quackmux erzeugt typisch ~90 und
höchstens ~140 Zeichen und lehnt alles ab, was länger würde.

* **Vertraulich:** Die Backend-Connection-ID ist ein Bearer-Credential für die Engine-Session. Sie gelangt nie im
  Klartext zum Client.
* **Authentisch:** Ein Client kann keine Route zu fremden Engines fälschen.
* **Selbstbeschreibend:** Jeder Gateway-Prozess mit demselben Schlüssel kann den Request routen, auch nach einem
  Neustart oder Leader-Failover ohne gemeinsamen Session-Store. Unbekannte, aber gültige IDs werden *adoptiert*
  (siehe 6).

### 4.3 Alle anderen Nachrichten

1. Header lesen. Er ist klein und wird per Peek auf die ersten Bytes gelesen. Typ prüfen: nur Client-Requests.
2. Route entsiegeln. Gelingt das nicht, antwortet das Gateway mit `ERROR_RESPONSE "Invalid connection id"`, mit
   identischem Text wie der Server.
3. Session suchen:
   * offen: weiter;
   * Tombstone, also vom Gateway geschlossen: präziser Fehler, z. B. „quackmux: session closed: idle timeout
     (6h0m0s) – DETACH and ATTACH again“;
   * unbekannt: adoptieren, wenn die Engine existiert und die Backend-Verbindung noch kennt. Das prüft das
     Gateway einmal per HEARTBEAT an die Engine, was zugleich die Engine-Lease erneuert. Weil das Gateway jede
     Session, die es schließt, bei der Engine abmeldet, kommen geschlossene Sessions so auch nach Ablauf ihres
     Tombstones oder nach einem Neustart nicht zurück (DuckDB-Clients senden weiter Heartbeats). Ist die
     wiedergefundene Engine gerade nicht ready oder nicht erreichbar, folgt ein vorübergehender Fehler ohne
     Invalidierung. Fehlt die Engine ganz, gibt es einen `must_invalidate`-Fehler „engine no longer exists“.
4. Aktivität verbuchen:
   * `lastSeen`: jede Nachricht.
   * `lastActive`: alle außer `HEARTBEAT_REQUEST`.
   * `inFlight++` bis zum Ende der Antwort.

   `HEARTBEAT_REQUEST` und `DISCONNECT_MESSAGE` beantwortet das Gateway **selbst**:
   * **Heartbeats:** Das Gateway besitzt die Client-Lease. Von der Engine fordert es eine längere Lease an
     (`session.backendHeartbeatTimeout`, Default 1 h) und erneuert sie selbst per Keepalive, spätestens nach
     einem Drittel davon. Das gilt auch, während ein Request läuft: Die Engine erneuert die Lease nur beim
     Eingang einer Nachricht, und eine lange Query bringt währenddessen keine. Gezählt wird ab dem Senden
     eines Requests, nicht ab seiner Antwort. Eine hängende Engine kann so nie den Heartbeat-Thread des
     Clients blockieren, und damit auch nicht sein `DETACH`.
   * **DISCONNECT:** Das Gateway bestätigt sofort, denn der Client wartet nur 2 s. Der Abbau der
     Engine-Verbindung läuft im Hintergrund.
5. Header neu schreiben (Backend-Connection-ID einsetzen, `client_query_id` übernehmen). Den Rest des Bodys
   unverändert weitergeben, `Content-Length` anpassen.
   * Die Engine (cpp-httplib) bricht Verbindungen ab, die mehr als 5 s stocken. Deshalb puffert das Gateway
     Requests bis 100 MiB und Responses bis 64 MiB komplett im Speicher. Das Gesamtbudget liegt bei 1 GiB. Damit
     bringt ein langsamer Client die Engine nie in den Timeout.
   * Größere Nachrichten oder ein erschöpftes Budget werden gestreamt.
   * Requests über 100 MiB, also über dem Engine-Limit, lehnt das Gateway sofort mit einem Quack-Fehler ab.
6. Antwort weitergeben. Ein FETCH kann serverseitig lange blockieren, deshalb gibt es kein Response-Timeout. Die
   ersten Bytes werden gepeekt:
   * `ERROR_RESPONSE` wird vollständig gelesen und ausgewertet. Bei „Invalid connection id“ oder
     „Connection heartbeat lease expired“ wird die Session tot markiert. Bei `must_invalidate` geht die Engine
     auf FAILED.
   * `SUCCESS_RESPONSE` auf `DISCONNECT_MESSAGE` schließt die Session („client disconnect“).
7. Transportfehler zur Engine:
   * Vor der Antwort: `ERROR_RESPONSE`. Ist die Engine weg, mit `must_invalidate`.
   * Mitten im Stream: Die Client-Verbindung wird hart abgebrochen (`http.ErrAbortHandler`), damit der Client
     keine gekürzte, aber gültig aussehende Antwort sieht.

## 5. Lebenszyklen

### 5.1 Session

```
           connect ok                     DISCONNECT ok / Fehler „Invalid connection id“
 (neu) ───────────────▶ OPEN ─────────────────────────────────────────────▶ CLOSED (Tombstone, TTL 1h)
                         │  idle timeout | max lifetime | Admin-Kill | Engine-Drain-Timeout
                         ├──────────────▶ CLOSING ── CANCEL(wildcard) + DISCONNECT an Engine ──▶ CLOSED
                         │  keine Nachricht seit heartbeatTimeout + heartbeatGrace (Client tot)
                         └──────────────▶ CLOSING ── DISCONNECT an Engine (räumt Backend auf) ──▶ CLOSED
```

* `session.idleTimeout` (Default 6 h, wie `kyuubi.session.idle.timeout`): keine Query-Nachricht und kein
  laufender Request für diese Dauer. Heartbeats zählen **nicht** als Aktivität.
* `session.maxLifetime` (Default aus): harte Obergrenze.
* Lease-Ablauf: keine Nachricht (auch kein Heartbeat) für `heartbeatTimeout + heartbeatGrace` bei
  `inFlight == 0`.
* Bei jedem Schließen wird die Session von der Engine abgemeldet (`sessions--`).

### 5.2 Engine

```
 Acquire ──▶ STARTING ──ready──▶ READY ──(maxLifetime | Spec geändert | Admin-Drain)──▶ DRAINING
               │  startupTimeout / Pod failed           │ idle ≥ idleTimeout(level) & sessions=0
               ▼                                         ▼                    sessions=0 oder drainTimeout
             FAILED ──(failedRetention)──▶ TERMINATING ◀──────────────────────────┘
                                              │ Pod gelöscht
                                              ▼
                                            GONE (aus Registry entfernt)
```

* `engine.idleTimeout` pro Share-Level: CONNECTION 0 (sofort), USER/GROUP/SERVER 30 min (wie
  `kyuubi.session.engine.idle.timeout`). Profile können überschreiben.
* `engine.startupTimeout` (Default 180 s, wie `kyuubi.session.engine.initialize.timeout`).
* `engine.maxLifetime` (Default aus): DRAINING nimmt keine neuen Sessions an. Neue Sessions bekommen eine neue
  Engine, bestehende laufen aus. Nach `drainTimeout` werden sie geschlossen.
* `engine.replaceOnSpecChange` (Default an): Jede Engine trägt den Hash ihrer gerenderten Spec. Ändert sich ein
  Profil, gehen alte Engines auf DRAINING. Das ergibt ein „Rolling Update“ ohne Abbruch laufender Sessions.
* Pod-Ereignisse kommen über einen Informer: Ready, Failed, gelöscht, `ImagePullBackOff` usw. Wird ein Pod extern
  gelöscht oder stürzt er ab, gehen seine Sessions auf CLOSED („engine terminated“). Folge-Requests bekommen
  `must_invalidate`.
* Die Pods haben `restartPolicy: Never`: Nach einem Absturz ist der In-Memory-Zustand ohnehin weg. Ein Neustart
  im selben Pod würde nur falsche Hoffnungen wecken.

## 6. Neustart und Hochverfügbarkeit

* **Engine-Discovery:** Beim Start listet das Gateway alle Pods mit `app.kubernetes.io/managed-by=quackmux,
  quackmux.io/instance=<instance>`. Key, Profil und Spec-Hash rekonstruiert es aus Labels und Annotationen, das
  Token aus dem Pod-Secret.
* **Session-Adoption:** Die Clients senden spätestens nach `timeout/3 · 1,2` einen Heartbeat. Die versiegelte ID
  enthält alles, um die Session neu anzulegen; vorher bestätigt die Engine, dass sie die Verbindung noch hat
  (siehe 4.3). Während `engine.recoveryGrace` (Default `maxHeartbeatTimeout/2 + 30 s`) werden wiedergefundene
  Engines nicht als idle beendet. Findet das Gateway mehrere READY-Engines für denselben Key (eine Engine, die
  ihren Startup-Timeout verpasst hat, neben ihrem Ersatz), nimmt es für neue Sessions immer die älteste und
  drained die anderen.
* **HA (optional, `ha.enabled`):** Mehrere Replikas machen Leader-Election über eine Kubernetes-`Lease`. Nur der
  Leader ist `ready` und bekommt über den Service Traffic. Verliert er die Lease, beendet sich der Prozess und
  startet als Follower neu. Der neue Leader übernimmt per Discovery und Adoption. Die Sessions überleben.
* **Laufende Requests beim Shutdown:** Das Gateway wartet bis zu `server.shutdownTimeout` auf laufende Requests.
  Kurz vor Ablauf (spätestens 5 s davor) beantwortet es Queries, die noch auf ihre Engine warten, mit einem
  Fehler („the gateway is shutting down“) und bricht sie auf der Engine per CANCEL ab. Ein Request, der beim
  Prozessende unbeantwortet bliebe, könnte libcurl auf einer neuen Verbindung erneut senden; die Query liefe
  dann ein zweites Mal. Offene FETCHes und Uploads dürfen dagegen abbrechen: libcurl wiederholt sie beim nächsten
  Gateway, und die Engine liefert denselben Batch noch einmal bzw. verwirft doppelte Uploads.
* Das Gateway beendet beim eigenen Shutdown **keine** Engines.

## 7. Engines in Kubernetes

* Pod `quack-<level>-<owner>-<rand>` mit Container `engine` (Image aus dem Profil, Default
  `quackmux/duckdb-engine:2.0.0`, siehe `images/engine`). Der Entrypoint führt Settings und Init-SQL aus und
  startet `quack_serve` auf `0.0.0.0:9494` über Plain HTTP. Das Token kommt aus der Umgebungsvariable
  `QUACK_TOKEN`.
* Token: 32 Zufallsbytes pro Engine, in einem Secret `<pod>-token`. Das Secret wird zuerst angelegt und danach
  per OwnerReference an den Pod gehängt, damit Kubernetes es per Garbage Collection mitlöscht. Verwaiste Secrets
  räumt der Reconciler ab.
* Readiness-Probe: `GET /` auf 9494. Der Quack-Server liefert dort sein Banner.
* Härtung:
  * Pod: `automountServiceAccountToken: false`, `runAsNonRoot`, `seccompProfile: RuntimeDefault`.
  * Container: alle Capabilities gedroppt, read-only Root-FS mit `emptyDir` für `/tmp`.
  * Netz: eine NetworkPolicy erlaubt Ingress auf 9494 nur vom Gateway.
* Profile formen den Pod: Ressourcen, Env, NodeSelector, Tolerations, Affinity, ServiceAccount, PullSecrets,
  Labels und Annotationen. Optional dient ein komplettes `podTemplate` als Basis, das Gateway ergänzt seine
  Pflichtfelder. Persistente Daten gehen über PVCs im Template, sinnvoll bei `USER`-Engines.
* RBAC (Namespace-scoped):
  * pods: get, list, watch, create, delete, patch
  * secrets: get, list, create, delete, patch
  * events: create
  * leases: für HA

Weitere Provider:

* `process` startet lokale DuckDB-CLI-Prozesse für die Entwicklung.
* `static` routet auf extern betriebene Quack-Server. Diese Engines sind *permanent*: Das Gateway beendet und
  drained sie nie. Schlagen `provider.static.failureThreshold` Health-Probes in Folge fehl (Default 3), nimmt
  das Gateway sie aus dem Verkehr und schließt ihre Sessions; ein erfolgreicher Probe holt sie zurück.
  `CONNECTION` ist mit `static` nicht möglich.

## 8. Authentifizierung, Optionen, Policy

* `auth.mode`:
  * `static`: YAML-Datei mit Benutzern, Gruppen und SHA-256-Hashes der Tokens; Hot-Reload.
  * `jwt`: HS*/RS*/ES*/EdDSA über Datei-Keys oder JWKS; Claims für User und Gruppen konfigurierbar.
  * `none`: nur Entwicklung; der Benutzer kommt aus der Option `user`. In den anderen Modi wird diese Option
    abgelehnt.
* Optionen, die Clients setzen dürfen:
  * `share_level` (`connection`, `user`, `group`, `server`)
  * `subdomain`
  * `profile`
  * `group`
  * `user` (nur bei `auth.mode=none`)
* Beispiele (Client):

  ```sql
  -- Variante A: Optionen im Token (funktioniert immer)
  CREATE SECRET (TYPE quack, TOKEN 'mein-token?share_level=connection&profile=large',
                 SCOPE 'quack:quackmux.example.com:9494');
  -- Variante B: Optionen als HTTP-Header (nicht mit ssl_fingerprint)
  CREATE SECRET (TYPE quack, TOKEN 'mein-token', SCOPE 'quack:quackmux.example.com:9494',
                 EXTRA_HTTP_HEADERS MAP {'X-Quackmux-Share-Level': 'server', 'X-Quackmux-Subdomain': 'bi'});
  ATTACH 'quack:quackmux.example.com:9494' AS lake;   -- ggf. (DISABLE_SSL true) ohne TLS
  ```
* Policy:
  * Profile: `allowedShareLevels`, `allowedUsers`, `allowedGroups`.
  * `overrides[]` (erster Treffer gewinnt): eigene Defaults, `allowedShareLevels`, `allowedProfiles` und
    `maxSessions` je Benutzer- oder Gruppenmatch.
  * Limits:
    * `maxSessions`
    * `maxSessionsPerUser`
    * `maxEngines`
    * `maxEnginesPerUser`: Engines, die Sessions des Benutzers gestartet oder (aus dem Warm-Pool) übernommen
      haben, über alle Share-Levels. Sonst könnte jeder Benutzer SERVER- oder GROUP-Engines für beliebige
      Subdomains starten, bis `maxEngines` alle anderen aussperrt.
    * `maxStartingEngines`
* Isolation: Bei `GROUP`/`SERVER` teilen sich Benutzer denselben Datenbankzustand und dieselben Rechte der
  Engine. Wer Mandantentrennung braucht, nutzt `USER` oder `CONNECTION`.

## 9. Betrieb

* **Admin-API** (eigener Port 9495, Bearer-Token):
  * `GET /api/v1/sessions[?user=]`
  * `GET|DELETE /api/v1/sessions/{id}`
  * `POST /api/v1/sessions/{id}/cancel`
  * `GET /api/v1/engines`
  * `POST /api/v1/engines/{id}/drain`
  * `DELETE /api/v1/engines/{id}`
  * `/healthz`, `/readyz`, `/metrics` (Prometheus)
* **Metriken:**
  * `quackmux_sessions{share_level,profile}` und `quackmux_sessions_opened_total`,
    `quackmux_sessions_closed_total{reason}`
  * `quackmux_engines{state,share_level,profile}` und `quackmux_engine_starts_total{result}`,
    `quackmux_engine_start_seconds`
  * `quackmux_requests_total{type,outcome}`, `quackmux_request_seconds{type}`, `quackmux_bytes_total{direction}`
  * `quackmux_auth_failures_total{reason}`
* **Audit-Log** (optional): jede Query (`PREPARE_REQUEST`) mit User, Session, Engine, `client_query_id` und
  gekürztem SQL als strukturierte Logzeile.
* **CORS**: wie der Stock-Server (`OPTIONS /quack`, `Access-Control-Allow-Origin`), für DuckDB-Wasm. Requests mit
  einem `Origin`-Header (Browser senden ihn bei jedem POST, die DuckDB-Clients nie) aus nicht erlaubten Origins
  lehnt das Gateway ab. Default ist `["*"]`, mit `auth.mode: none` aber keiner: Sonst könnte jede Webseite über
  den Browser eines Benutzers, der das Gateway erreicht, Queries ausführen.

## 10. Bekannte Grenzen

* Läuft eine Query während eines Gateway-Restarts oder -Failovers, bekommt der Client einen Fehler, und die Query
  wird auf der Engine abgebrochen (siehe 6). Die Session bleibt erhalten.
* Optionen per Header funktionieren nicht mit `ssl_fingerprint`-Pinning. Dieser Client-Pfad sendet keine
  Extra-Header. Dort Token-Optionen nutzen.
* Das Gateway prüft kein SQL. Autorisierung innerhalb geteilter Engines bleibt Sache der Engine (Init-SQL,
  Rechte der angehängten Daten) oder der Wahl des Share-Levels.
* Der Client verlangt eine exakte Protokollversion (3). Neue Quack-Versionen erfordern ein Gateway-Update. Das
  Gateway lehnt unbekannte Felder im Handshake ab, statt sie still zu verfälschen.
