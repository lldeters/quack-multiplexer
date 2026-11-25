# Übergabe: quackmux weiterbauen (Stand 2026-11-22)

Diese Datei ist der Übergabeprompt für eine neue Claude-Code-Session in der Cloud. Sie ist vollständig und
braucht keinen weiteren Kontext.

---

## Auftrag (Originalanforderung)

> Idealerweise hätten wir eine Art Kyuubi für Quack: Man kann mit Quack verbinden, und über Optionen werden die
> Anfragen auf einen gesharten Pod oder individuelle Pods gemultiplext. Das Session Management (Timeout,
> Aufräumen der Pods etc.) hätte eine gewisse Komplexität.

Quack ist das HTTP-Client-Server-Protokoll von DuckDB (seit 1.5.3, stabil in 2.0.0 LTS). Dieses Repo
(**quackmux**) ist ein Gateway in Go 1.26: Clients machen `ATTACH 'quack:gateway:9494'`, das Gateway
authentifiziert sie und routet jede Verbindung je nach Share-Level (CONNECTION/USER/GROUP/SERVER, wie bei Apache
Kyuubi) auf Engine-Pods mit DuckDB und `quack_serve`. Diese Pods startet und beendet es selbst.

**Zuerst lesen:** [README.md](../README.md), [docs/design.md](design.md), [docs/protocol.md](protocol.md),
[docs/research-notes.md](research-notes.md). Die verifizierten Fakten, auf denen das Design beruht, stehen in
den Research-Notes, die gegen DuckDB v2.0.0 geprüften im Abschnitt „Verified against the released DuckDB v2.0.0“.

## Stand

**Verifiziert am 2026-11-20 und 2026-11-21 unter linux/amd64** (Cloud-Sandbox mit Docker und kind):

* `go build ./...`, `go vet ./...` und `gofmt -l .` sind sauber. `go test ./... -count=1` und
  `go test -race ./...` sind grün; drei Race-Läufe hintereinander ohne Befund.
* Gegen das veröffentlichte DuckDB v2.0.0 (`d54cbbe3df`, quack `f96420ed46`):
  * Engine- und Gateway-Image gebaut (`make docker-engine docker-gateway`), der Selbsttest des Engine-Images
    besteht.
  * `test/e2e/compose/run.sh`: alle Szenarien grün (seit 2026-11-25 a–k, darunter Query-Zeitlimit, gehärtete Engine und verwaiste
    Verbindungen).
  * `test/e2e/kind/setup.sh` und `test.sh` (Kubernetes v1.37, Overlay `dev`): alle Szenarien grün (seit
    2026-11-22 k1–k8), auch der Gateway-Neustart mit Session-Adoption und die persistenten Profile.
  * `test/e2e/kind/ha.sh` (Overlay `ha`, zwei Replikas, echte Secrets): alle Szenarien grün (seit 2026-11-22 h1–h6
    und h4b). Geprüft werden Leader-Wahl, geordnete Übergabe, ein eingefrorener und wieder aufgeweckter Leader,
    Rollout, ein hängender Rollout, PodDisruptionBudget und ein persistenter Schlüssel beim Failover, jeweils mit
    zwei angehängten Clients.
  * Wire-Codec: 400 mitgeschnittene Nachrichten (Client ↔ Server, Client ↔ Gateway, Gateway ↔ Engine) dekodieren
    und kodieren byte-genau (`internal/wire/testdata/duckdb-v2.0.0`, `realtraffic_test.go`).
  * Der `process`-Provider läuft mit der echten CLI.
* Adversariales Gesamt-Review aus fünf Blickwinkeln (Protokoll, Nebenläufigkeit, Fehlerfälle, Security,
  Kubernetes): 36 Funde nach Deduplizierung. Jeder Fund wurde mit einem PoC-Test (meist gegen den Code, teils
  gegen echtes DuckDB) gegengeprüft. Alle sind behoben oder als Grenze dokumentiert, jeder Fix hat einen
  Regressionstest. Kurzfassung unten.

| Bereich | Pfad | Status |
|---|---|---|
| Wire-Codec (Header, Handshake, Fehler, Kontrollnachrichten) | `internal/wire` | fertig, gegen Mitschnitte von DuckDB v2.0.0 geprüft |
| Proxy (Handshake, Routing, Header-Rewrite, Puffern/Streaming, lokale Heartbeats/DISCONNECT, Audit) | `internal/proxy` | fertig, Integrationstests gegen den Fake-Server `internal/quacktest`, E2E gegen DuckDB v2.0.0 |
| Sessions (versiegelte IDs ≤ 140 Zeichen, Timeouts, Lease, Keepalive, Adoption, Tombstones) | `internal/session` | fertig |
| Engine-Manager (Single-Flight-Start, Pools, Warm-Pool, Drain, Spec-Drift, Recovery-Grace, permanente Static-Engines) | `internal/engine` | fertig |
| Provider: Kubernetes, process (lokal), static, fake | `internal/engine/*` | fertig; Kubernetes in kind, process mit echter CLI geprüft |
| Auth: static (SHA-256-Token-Datei, Hot-Reload, SIGHUP), jwt (HS/RS/ES/EdDSA, JWKS), none | `internal/auth` | fertig |
| Policy (Optionen, Defaults, Overrides, Rechte), Config (YAML-Schema, Defaults, Validierung) | `internal/policy`, `internal/config` | fertig |
| Admin-API, Metriken, Leader-Election | `internal/admin`, `internal/metrics`, `internal/ha` | fertig; Leader-Election in kind mit Failover geprüft |
| Binary | `cmd/quackmux` (`serve`, `validate`, `token`, `hash-token`, `version`) | fertig |
| Engine-Image (tini, FIFO-Entrypoint, quack/httpfs vorinstalliert), Gateway-Image | `images/engine`, `Dockerfile` | gebaut und mit DuckDB v2.0.0 geprüft |
| Kustomize (base, overlays `dev` und `ha`), Beispielkonfigurationen | `deploy/kubernetes`, `examples` | `dev` und `ha` in kind deployt und getestet; `base` per `kubectl apply --dry-run=server` geprüft |
| E2E-Tests (Docker Compose, kind, kind-HA) | `test/e2e` | ausgeführt und grün (siehe oben) |

## Harte Randbedingungen

Diese Regeln nicht brechen:

1. **Gateway-Connection-IDs ≤ 156 Bytes.** Sonst wirft der DuckDB-Client beim INSERT eine `InternalException` und
   invalidiert seine gesamte Datenbank (`quack_send_data.cpp:99`). `session.MaxIDLength` ist auf 140 gesetzt,
   ein Test sichert das ab. Beobachtet wurden 88 bis 90 Zeichen.
2. **Fehler immer als HTTP 200 mit `ERROR_RESPONSE`.** Ein schon gelesener Request bekommt immer eine Antwort.
   Ein Verbindungsabbruch ohne Antwort kann libcurl einen POST wiederholen lassen, dann läuft eine Query doppelt.
   Das gilt auch beim Herunterfahren (`Gateway.AbortQueries`).
3. **Routing nur über die `connection_id` im Header**, nie über TCP-Verbindungen. Clients schicken bis zu 256
   parallele FETCH-Long-Polls pro Session.
4. **Keine kurzen Timeouts auf PREPARE, FETCH oder SEND_DATA.** Sie blockieren serverseitig beliebig lange, der
   Client wartet bis zu 24 h. Kurze Deadlines gelten nur vor der Authentifizierung
   (`server.readHeaderTimeout`) und für den Handshake zur Engine (`server.upstream.handshakeTimeout`).
5. **Puffern statt durchstreamen** (bis 100 MiB Request, 64 MiB Response, Budget 1 GiB). Die Engine bricht
   Verbindungen ab, die 5 s stocken.
6. **Client-Heartbeats und DISCONNECT beantwortet das Gateway lokal.** Die Engine-Lease (1 h) erhält es selbst
   per Keepalive, auch während langer Requests.
7. **Fremde oder unbekannte Connection-IDs nie weiterleiten.** Nur versiegelte IDs (AES-GCM) werden geroutet.
8. Doku für Nutzer und Betreiber auf Deutsch, Code-Kommentare auf Englisch.

## Was das Review geändert hat

Die Commits auf dem Branch erklären jeden Fix im Detail; hier die Übersicht:

* **Engine-Lease:** Das Gateway hält die Lease der Backend-Verbindung auch während langer Requests am Leben
  (vorher lief sie nach 1 h ab, und die Session war tot).
* **Engine-Lebenszyklus:** Nach einem Neustart wiedergefundene Engines starten nicht neu in den Startup-Timeout,
  gescheiterte Launches bleiben nicht ewig TERMINATING, der Warm-Pool füllt mit Backoff nach. Gibt es nach einem
  Neustart zwei Engines für einen Schlüssel, bekommen neue Sessions deterministisch die ältere, die andere geht
  auf DRAINING.
* **Sessions:** Eine adoptierte Session gilt erst nach einem bestätigenden Heartbeat bei der Engine als
  übernommen. Vom Gateway geschlossene Sessions kehren nicht als Geister zurück. Backend-Verbindungen werden auch
  dann mit DISCONNECT geschlossen, wenn der Client mitten im Handshake verschwindet.
* **Limits:** `limits.maxEnginesPerUser` zählt jede Engine, die ein Benutzer verursacht hat: auch `SERVER`- und
  `GROUP`-Engines für neue Subdomains, Warm-Claims und geclaimte Warm-Engines nach einem Neustart.
* **HTTP-Seite:** Nicht authentifizierte Requests haben eine Read-Deadline. Requests mit einem `Origin` außerhalb
  von `server.allowedOrigins` werden abgelehnt; mit `auth.mode: none` ist standardmäßig kein Origin erlaubt.
  Fehlerantworten der Engine zählen zum Puffer-Budget. Beim Herunterfahren bekommen wartende Queries eine
  Fehlerantwort und werden auf der Engine abgebrochen.
* **Kubernetes:**
  * Das Init-SQL liegt im Engine-Secret statt in der Pod-Spec.
  * Literale Env-Werte sind gegen die `$(VAR)`-Expansion des Kubelets escaped.
  * Engines tolerieren einen verlorenen Node 60 s statt 300 s.
  * Die NetworkPolicy sperrt den Metadaten-Endpunkt, und Engines erreichen den Admin-Port nicht.
  * Engine-Container starten nie in place neu.
  * `QUACKMUX_STOP_TIMEOUT` folgt der Grace Period.
  * Image und Pull-Policy aus dem `podTemplate` wirken.
* **Security und Betrieb:**
  * Die öffentlichen Beispiel-Secrets werden abgelehnt.
  * Die Admin-API lauscht ohne Token nur auf Loopback, eine leere Token-Datei ist ein Fehler.
  * Das Audit-Log schwärzt Statements mit Zugangsdaten.
  * `hash-token` liest von stdin.
  * Der static-Provider nimmt eine Engine erst nach `failureThreshold` Fehlschlägen aus der Rotation.
  * `quackmux validate` prüft Provider und Leader-Election ohne Cluster.

## Was der HA-Test gefunden hat

`test/e2e/kind/ha.sh` hat vier Fehler gefunden, die die Unit-Tests nicht zeigten. Alle sind behoben, mit
Regressionstests:

* **Ein aufgeweckter Leader schloss fremde Sessions.** Ein eingefrorener Leader (SIGSTOP) sah nach dem Aufwachen
  die Heartbeats seiner Sessions fehlen, weil sie beim neuen Leader angekommen waren. Er schloss die Sessions
  mit `DISCONNECT` an die Engine; der Client mit kurzer Heartbeat-Lease war danach tot. Jetzt handeln
  Session-Reaper und Engine-Reconciler nur mit frischer Lease (`ha.Elector.Fresh`). Nach einer Prozesspause
  überspringt der Reaper eine Runde, auch ohne HA.
* **Derselbe Leader gab die Lease des neuen Leaders frei.** client-go 0.37 (`tryRelease`) liest die Lease und
  entscheidet dann nach dem zuletzt beobachteten Stand. Das Update ging ohne Konflikt durch. Folge: ein zweiter,
  unnötiger Failover und kurz zwei Prozesse, die sich für den Leader hielten. Der Lock-Wrapper schreibt eine
  Freigabe jetzt nur, wenn die gerade gelesene Lease uns gehört.
* **Beim geordneten Herunterfahren lief die Lease ab, statt freigegeben zu werden.** Der Prozess endete, bevor
  der Elector die Freigabe geschrieben hatte. `runCtx` hing am Signal-Kontext, Fund R war in diesem Punkt nicht
  wirklich behoben. Die Übergabe dauerte 17–22 s statt 1–4 s. Jetzt gibt der Leader die Lease zuerst ab,
  wartet darauf und lässt danach die laufenden Requests auslaufen. Ohne HA stoppt die Hintergrundarbeit erst
  nach dem Drain.
* **Der dokumentierte Rollout konnte hängen bleiben.** Die alte ReplicaSet ersetzt den gelöschten Leader durch
  einen Pod mit altem Template. Der gewann die Lease mitunter gegen den neuen Follower, und der Rollout stand
  wieder. Ein frisch gestarteter Prozess bewirbt sich jetzt erst nach `3 × ha.retryPeriod`.

Nebenbei: Ein Gateway im Shutdown meldet sich jetzt mit dem Signal sofort nicht mehr ready.

## Bekannte Grenzen

1. **Verwaiste Backend-Verbindungen nach einem Gateway-Neustart** (`engine.closeOrphanedConnections`, Default an,
   bei `static` aus): Das Gateway schließt Verbindungen toter Clients erst, wenn ihre Engine-Lease abgelaufen ist,
   mit den Defaults gut eine Stunde nach der Übernahme; so trifft es nie einen lebenden Client. Gehärtete Engines
   räumt es nur mit der Makro-Variante `quackmux_authorize_listing` auf (sie zeigt jeder Session die
   Verbindungs-IDs, passt also nur, wenn allein das Gateway den Engine-Port erreicht). Engines mit mehr als 2048
   Verbindungen räumt es nicht auf (`quack_active_connections()` von DuckDB v2.0.0 scheitert dann).
2. **Persistente Daten pro Engine-Schlüssel** (`persistence` im Profil, ein PVC je Schlüssel, nie zwei Engines
   eines Schlüssels gleichzeitig): Ein erzwungenes Löschen eines Pods, ein Node-Ausfall, ein eigener Scheduler
   und mit HA ein Restfenster von Sekunden umgehen die Prüfungen des Gateways; dann schützen nur
   `ReadWriteOncePod` und die Dateisperre von DuckDB. `kubectl delete -k` löscht mit dem Namespace alle Claims.
   Alles in `deploy/kubernetes/README.md`, „Persistente Daten“.
3. **HA:** Alle gesunden Replikas sind ready, nur der Leader bekommt Quack-Traffic (Label `quackmux.io/leader=true`,
   der Service von `overlays/ha` wählt es aus). Bei einer geordneten Übergabe hat der Service 0,4–3,6 s keinen
   Endpunkt, nach einem eingefrorenen Leader übernimmt der Follower nach 15–20 s (in kind gemessen); Requests in
   der Lücke scheitern mit „Could not connect to server“ und laufen nicht. Eigene Services, die Quack-Traffic
   routen, brauchen denselben Selector. Ein Upgrade von einer Version ohne Label lässt den Service während des
   Rollouts ohne Endpunkt. Alles in `deploy/kubernetes/README.md`, „Hochverfügbarkeit“.
4. **Härtung geteilter Engines** (`postStartSQL` mit `enable_external_access = false` und
   `lock_configuration = true`, dazu das Makro `images/engine/authorize.sql`): Das Makro ist eine Textprüfung
   (Deny-Liste) und trennt keine Daten. Sessions einer Engine teilen Datenbanken und Spill-Dateien, Zugangsdaten
   in ATTACH-URLs oder View-Definitionen aus dem Init-SQL sind über den Katalog lesbar, und eine gehärtete Engine
   lädt keine Extensions mehr nach. Ohne Härtung kann jede Session `quack_stop(…)` aufrufen und das Engine-Token
   lesen. Alles in `deploy/kubernetes/README.md`, „Härtung geteilter Engines“.
5. **Die Schwärzung im Audit-Log ist eine Heuristik** (Schlüsselwörter wie `secret`, `passw`, `token`, URLs mit
   Passwort). Das Audit-Log bleibt vertraulich.
6. **Spill-Platz** begrenzt nur die Konfiguration (`settings.max_temp_directory_size` plus
   `resources.limits.ephemeral-storage`); die Beispiele setzen keine Defaults.

## Nächste Schritte (priorisiert)

Erledigt seit dem 2026-11-21: CI und Release mit GitHub Actions (`.github/workflows`, [docs/releasing.md](releasing.md)),
der HA-Test nächtlich in CI, PVCs pro Engine-Schlüssel (`persistence`, kind-Szenarien k5–k8 und h6), HA mit
allen Replikas ready (Leader-Label, `ha.deregistrationDelay`, kind-Szenarien h1–h5 und h4b), die Härtung
geteilter Engines (`postStartSQL`, Autorisierungs-Makro mit 224 Testfällen im Selbsttest des Images; dabei
zwei Lecks im Engine-Image geschlossen: das Engine-Token über `FROM _` und Befehle über `/proc/self/fd/0`),
ein optionales Zeitlimit pro Query (`limits.queryTimeout`, CANCEL auf der Engine, Compose-Szenario h) und das
Aufräumen verwaister Verbindungen nach einem Neustart (`engine.closeOrphanedConnections`, Compose-Szenario k).

1. Optional:
   * Helm-Chart
   * TLS-Doku für Clients mit `ssl_fingerprint` (die haben nur 30 s `http_timeout`, deshalb Warm-Pool nutzen)
   * Spill-Defaults in den Beispielprofilen

## Arbeiten im Repo

```bash
go build ./... && go vet ./... && go test ./... -count=1
go test -race ./...
go run ./cmd/quackmux validate -config examples/config.local.yaml
make help   # oder Makefile ansehen: build, test, docker-engine, docker-gateway, e2e-compose, kind-up/test/down
```

* Die Unit- und Integrationstests brauchen weder Docker noch DuckDB. `internal/proxy/harness_test.go` baut ein
  komplettes Gateway mit Fake-Engines und Fake-Uhr.
* E2E: `test/e2e/compose/run.sh` und `test/e2e/kind/setup.sh` plus `test.sh` (Overlay `dev`) und `ha.sh`
  (Overlay `ha`, stellt danach `dev` wieder her); Voraussetzungen und Variablen in
  [test/e2e/README.md](../test/e2e/README.md).
  * Hinter einem Proxy oder mit Mirrors reicht `DOCKER_BUILD_FLAGS` Flags an beide `docker build` durch (etwa
    `--network host --build-arg GOPROXY=…`). Das Engine-Image nimmt `DUCKDB_EXTENSION_REPOSITORY` als Build-Arg.
  * Auf Hosts mit cgroup v1 braucht kind eine eigene Clusterkonfiguration (`KIND_CONFIG`, siehe die
    Fehlersuche in test/e2e/README.md).
* Der Quellcode von duckdb-quack liegt unter `https://github.com/duckdb/duckdb-quack`. DuckDB v2.0.0 enthält
  `f96420ed46`; recherchiert wurde an `02c9d4a`, die Nachrichten sind identisch. Der Serializer von DuckDB liegt
  in `duckdb/duckdb@181615215e5e/src/common/serializer/`.

---

**Prompt für die nächste Cloud-Session** (zum Einfügen):

> Lies `docs/HANDOVER.md` und die dort verlinkten Dokumente. Arbeite dann die „Nächsten Schritte“ in der
> angegebenen Reihenfolge ab, beginnend mit CI und Release. Halte dich strikt an die „Harten Randbedingungen“.
> Committe in kleinen Schritten auf einem Feature-Branch und antworte mir auf Deutsch.
