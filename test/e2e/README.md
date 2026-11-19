# End-to-End-Tests

Diese Tests prüfen quackmux mit **echten DuckDB-2.0-Clients und -Engines**. Es gibt zwei Umgebungen:

| Umgebung | Verzeichnis | Was läuft | Dauer |
|---|---|---|---|
| Docker Compose | [compose](compose) | Gateway mit Provider `static` vor zwei Engine-Containern, Auth `static` | ca. 1 min (plus Build) |
| kind | [kind](kind) | Overlay `deploy/kubernetes/overlays/dev`: Das Gateway startet Engine-Pods selbst | einige Minuten |

Die Unit- und Integrationstests (`go test ./...`) brauchen weder Docker noch DuckDB. Das gilt auch für die
Go-Tests in diesem Verzeichnis: Sie prüfen nur die Fixtures des Compose-Tests (siehe
[Konsistenz der Fixtures](#konsistenz-der-fixtures)).

## Voraussetzungen

* Linux, macOS oder Windows mit WSL2. Die Skripte brauchen `bash` (ab 3.2), `curl`, `awk`, `sed`, `grep` und
  `mkfifo`. Git Bash unter Windows ist nicht vorgesehen.
* Docker mit Compose v2 (`docker compose`).
* Internetzugang beim ersten Build: Das Engine-Image ([images/engine](../../images/engine)) lädt die DuckDB-CLI
  2.0.0 von GitHub und installiert die Extensions `quack` und `httpfs`, das Gateway-Image lädt Go-Module. Die Tests
  selbst laufen danach offline (`autoinstall_known_extensions = false`).
* Für kind zusätzlich `kind` und `kubectl` (mit eingebautem Kustomize, `kubectl kustomize`).
* Freie Ports auf `127.0.0.1`: 19494, 19495, 19594 und 19595 (Compose) sowie 19695 (kind, Port-Forward). Alle
  lassen sich über Variablen ändern.

## Docker Compose

```bash
bash test/e2e/compose/run.sh
```

`run.sh` baut die Images `quackmux/duckdb-engine:2.0.0` (aus `images/engine`) und `quackmux/quackmux:e2e` (aus
dem `Dockerfile` im Repository), startet die Umgebung und wartet, bis beide Gateways auf `/readyz` antworten und
beide Engines als `READY` melden. Danach laufen die Szenarien. Ein fehlgeschlagenes Szenario bricht den Lauf nicht
ab: Am Ende stehen eine Zusammenfassung, die Ausgabe der betroffenen DuckDB-Clients und die letzten Zeilen aller
Container-Logs. Der Exit-Status ist 0 nur, wenn alle Szenarien bestehen. Aufgeräumt wird immer
(`docker compose down -v`), auch nach einem Fehler oder Strg+C.

### Aufbau

| Service | Image | Rolle |
|---|---|---|
| `engine-shared` | Engine-Image | DuckDB-2.0-Quack-Server für Share-Level `SERVER`. Das Init-SQL legt `whoami` mit `engine = 'shared'` an. |
| `engine-analytics` | Engine-Image | Dasselbe für Share-Level `GROUP` der Gruppe `analytics` (`engine = 'analytics'`). |
| `gateway` | Gateway-Image | quackmux mit [config/gateway.yaml](compose/config/gateway.yaml): Provider `static` auf beide Engines, Auth `static` mit [config/users.yaml](compose/config/users.yaml), Admin-API mit Bearer-Token. |
| `gateway-idle` | Gateway-Image | Wie `gateway`, aber [config/gateway-idle.yaml](compose/config/gateway-idle.yaml) mit `session.idleTimeout: 15s` und Prüfintervallen von 1 s. |
| `client` | Engine-Image | Die DuckDB-CLI (`/usr/local/bin/duckdb` als Entrypoint, Profil `client`). Jeder Client ist ein eigener `docker compose run`. |

Engines und Gateways laufen wie ihre Pods in Kubernetes mit read-only Root-Dateisystem, ohne Capabilities und mit
einem tmpfs auf `/tmp`. Die Engines erhalten ihre Einstellungen über den Engine-Container-Vertrag
(`QUACK_TOKEN`, `QUACKMUX_INIT_SQL` usw., siehe `internal/engine/initsql.go`).

Alle Zugangsdaten sind Testwerte aus [compose/e2e.env](compose/e2e.env). `users.yaml` enthält nur die SHA-256-Hashes
der Client-Tokens (mit Go berechnet, identisch mit `quackmux hash-token TOKEN`), `config/admin.token` den Token der
Admin-API, die Gateway-Konfigurationen die Engine-Tokens.

| Benutzer | Gruppen | Verwendung |
|---|---|---|
| `alice` | `analytics`, `engineering` | darf die Gruppen-Engine `analytics` nutzen |
| `bob` | `reporting` | ist **kein** Mitglied von `analytics` |

### Szenarien

Die Clients setzen ihre Optionen im Token, etwa `TOKEN 'e2e-alice-…?share_level=group&group=analytics'`, und
hängen das Gateway mit `ATTACH 'quack:gateway:9494' AS remote (DISABLE_SSL true)` an.

| | Ablauf | Was es beweist |
|---|---|---|
| a | alice, `share_level=server`: liest `remote.whoami`, legt per `remote.query('CREATE TEMP TABLE …')` eine temporäre Tabelle an, liest sie in der nächsten Query, `DETACH`. Die Admin-API zeigt danach keine Session mehr. | Handshake mit Token-Ersetzung, Routing `SERVER` → `engine-shared`, Umschreiben der Connection-ID: Alle Requests einer Session erreichen dieselbe Backend-Verbindung, sonst wäre die temporäre Tabelle unsichtbar. `DETACH` schließt die Session im Gateway. |
| b | bob, `share_level=group&group=analytics` | Die Policy prüft die Gruppenmitgliedschaft. `ATTACH` scheitert mit „not a member“, es entsteht keine Session. |
| c | alice, `share_level=group&group=analytics`: liest `whoami` | Routing `GROUP` → Engine der Gruppe (`whoami = analytics`). |
| d | unbekannter Token | `ATTACH` scheitert mit „Authentication failed“ (Text wie beim Stock-Server). `quackmux_auth_failures_total` zählt mit. |
| e | bob hält ein Attachment offen (der Client liest weiter von einer FIFO). Währenddessen: `GET /api/v1/sessions?user=bob`, `GET /api/v1/sessions/{id}`, ein Request ohne Token. Danach `DETACH`. | Session-Tracking und Admin-API (Session `OPEN`, Share-Level `SERVER`, Engine `READY`), Bearer-Authentifizierung (401 ohne Token), Abmelden beim `DETACH`. |
| f | alice über `gateway-idle`: Query, 20 s Pause, Query | Der Idle-Reaper schließt die Session nach 15 s ohne Query, obwohl der Client weiter Heartbeats schickt. Die nächste Query bekommt den Grund („idle timeout“), `quackmux_sessions_closed_total{reason="idle timeout"}` zählt mit. |
| g | `CREATE TABLE remote.e2e_range AS SELECT range FROM range(100000)` und `CREATE TABLE remote.e2e_upload AS SELECT id FROM e2e_local` (lokale Tabelle), danach `count(*)` und `sum(id)` | Der Client streamt die Zeilen mit `SEND_DATA_REQUEST` durch das Gateway (`quackmux_requests_total{type="SEND_DATA_REQUEST",outcome="ok"}` steigt). 100 000 Zeilen kommen vollständig und unverfälscht an. |
| h | Logs beider Gateways | Kein Request-Handler ist mit einem Panic abgestürzt. |

### Variablen

| Variable | Default | Bedeutung |
|---|---|---|
| `E2E_SKIP_BUILD` | – | `1`: vorhandene Images verwenden, nicht bauen |
| `E2E_KEEP` | – | `1`: Container nach dem Lauf weiterlaufen lassen (zum Untersuchen) |
| `E2E_ENGINE_IMAGE` | `quackmux/duckdb-engine:2.0.0` | Engine- und Client-Image |
| `E2E_GATEWAY_IMAGE` | `quackmux/quackmux:e2e` | Gateway-Image |
| `E2E_EXTENSION_DIR` | `/opt/duckdb/extensions` | Extension-Verzeichnis im Engine-Image; leer: DuckDB-Default |
| `E2E_QUACK_PORT`, `E2E_ADMIN_PORT` | 19494, 19495 | Host-Ports von `gateway` |
| `E2E_IDLE_QUACK_PORT`, `E2E_IDLE_ADMIN_PORT` | 19594, 19595 | Host-Ports von `gateway-idle` |
| `NO_COLOR` | – | gesetzt: Ausgabe ohne Farben |

### Von Hand ausprobieren

```bash
cd test/e2e/compose
E2E_KEEP=1 bash run.sh        # oder: docker compose --env-file e2e.env up -d (nach dem Build)
curl -H "Authorization: Bearer $(cat config/admin.token)" http://127.0.0.1:19495/api/v1/sessions
docker compose -p quackmux-e2e --env-file e2e.env run --rm -T --no-deps client <<'SQL'
SET extension_directory = '/opt/duckdb/extensions';
LOAD quack;
CREATE SECRET (TYPE quack, TOKEN 'e2e-alice-3f9c2b7d1a8e4c60?share_level=server', SCOPE 'quack:gateway:9494');
ATTACH 'quack:gateway:9494' AS remote (DISABLE_SSL true);
FROM remote.whoami;
SQL
docker compose -p quackmux-e2e --env-file e2e.env down -v
```

Eine lokale DuckDB-2.0-CLI erreicht das Gateway unter `quack:localhost:19494` (lokale Adressen nutzen ohnehin
HTTP).

## kind

```bash
bash test/e2e/kind/setup.sh      # Cluster "quackmux", Images bauen und laden, Overlay dev ausrollen
bash test/e2e/kind/test.sh       # Szenarien k1–k4 (beliebig oft wiederholbar)
bash test/e2e/kind/teardown.sh   # Cluster löschen
```

Alle Skripte verwenden den Kontext `kind-quackmux` explizit und ändern den aktuellen kubectl-Kontext nicht.

**setup.sh** legt den Cluster an, falls er fehlt, baut Engine- und Gateway-Image und lädt beide mit
`kind load docker-image`. Die Image-Namen liest das Skript aus dem gerenderten Overlay
(`quackmux/quackmux:dev`, `quackmux/duckdb-engine:2.0.0`). Dann folgen `kubectl apply -k deploy/kubernetes/overlays/dev`
und das Warten auf den Rollout. Bei einem erneuten Aufruf startet es das Gateway neu und löscht alte Engine-Pods,
damit alles mit den frisch geladenen Images läuft. Es warnt, wenn das Overlay nicht `auth.mode: none` nutzt oder
keinen Session-Key konfiguriert.

**test.sh** startet den Pod `quackmux-e2e-client` mit der DuckDB-CLI des Engine-Images. Darin laufen die Clients
per `kubectl exec` und sprechen das Gateway über seinen Service an (`quack:quackmux:9494`). Die Admin-API erreicht
das Skript per `kubectl port-forward` auf `127.0.0.1:19695`. Das Dev-Overlay nutzt `auth.mode: none`, der Benutzer
kommt also aus der Token-Option (`TOKEN 'e2e?user=e2e-alice-<run-id>&share_level=connection'`). Jeder Lauf verwendet
neue Benutzernamen mit der Run-ID und beendet die Engines, die er gestartet hat, über
`DELETE /api/v1/engines/{id}`.

| | Ablauf | Was es beweist |
|---|---|---|
| k1 | Zwei Sessions mit `share_level=connection`, gleichzeitig offen; danach `DETACH` der ersten | Jedes `ATTACH` bekommt einen eigenen Engine-Pod (`kubectl get pods -l app.kubernetes.io/component=engine` zeigt zwei neue Pods). Nach dem `DETACH` verschwindet der Pod der ersten Session binnen 30 s, der zweite läuft weiter. Das prüft `engine.idleTimeout.CONNECTION = 0`, das Löschen durch den Provider und das Stoppen der Engine bei SIGTERM. |
| k2 | Zwei Sessions desselben Benutzers mit `share_level=user`; die erste legt eine Tabelle an, die zweite liest sie | Beide Sessions teilen sich genau einen Engine-Pod und dessen Datenbank. |
| k3 | Der Engine-Pod einer `USER`-Session wird gelöscht. Danach eine Query auf demselben Attachment, dann `DETACH` und erneutes `ATTACH` | Das Gateway erkennt den Verlust über den Informer und schließt die Session. Die nächste Query scheitert mit „session closed“ statt zu hängen. Ein neues `ATTACH` startet einen neuen Pod und funktioniert. |
| k4 | Eine offene Session, dann `kubectl rollout restart` des Gateways, danach eine Query auf demselben Attachment | Das neue Gateway findet die Engine-Pods wieder (Discovery) und übernimmt die Session aus der versiegelten Connection-ID (Adoption): Die Query klappt, die Admin-API zeigt die Session mit `"adopted": true` auf derselben Engine. Das Dev-Overlay nutzt die Strategie `Recreate`; dass Heartbeats in der kurzen Pause scheitern, toleriert der Client. Scheitert die erste Query nach dem Neustart mit einem Transportfehler, wiederholt das Skript sie bis zu zweimal und meldet das als Warnung. |

Anforderungen an das Overlay (das Dev-Overlay erfüllt sie): `auth.mode: none`, ein gemeinsamer Session-Key
(`server.sessionKeyFile` oder `QUACKMUX_SESSION_KEY`, sonst scheitert k4 mit „Invalid connection id“), ein Service mit
Port 9494, Platz für mindestens zwei gleichzeitige Engines (`limits.maxEngines`), auch für denselben Benutzer
(`limits.maxEnginesPerUser`), Pool-Größe 1 und ein kurzes `engine.checkInterval` (Default 5 s).

| Variable | Default | Bedeutung |
|---|---|---|
| `KIND_CLUSTER` | `quackmux` | Name des kind-Clusters |
| `KUBE_CONTEXT` | `kind-$KIND_CLUSTER` | kubectl-Kontext |
| `OVERLAY_DIR` | `deploy/kubernetes/overlays/dev` | Kustomize-Overlay |
| `ENGINE_IMAGE`, `GATEWAY_IMAGE` | aus dem Overlay | Image-Namen |
| `QMX_NAMESPACE`, `QMX_DEPLOYMENT` | aus dem Overlay | Namespace und Deployment des Gateways |
| `QMX_SERVICE`, `QMX_SERVICE_PORT` | Service mit Port 9494 | Ziel der Clients |
| `SKIP_BUILD` | – | `1`: Images nicht bauen, nur laden (setup.sh) |
| `KIND_NODE_IMAGE` | kind-Default | Node-Image für `kind create cluster` |
| `ADMIN_TOKEN` | – | Bearer-Token, falls die Admin-API einen verlangt |
| `ADMIN_LOCAL_PORT` | 19695 | lokaler Port des Port-Forwards |
| `CLIENT_UID` | 1000 | Benutzer des Client-Pods (der Benutzer des Engine-Images) |
| `ATTACH_TIMEOUT` | 180 | Sekunden für ein `ATTACH` inklusive Start eines Engine-Pods |
| `POD_GONE_TIMEOUT` | 30 | Sekunden, bis der `CONNECTION`-Pod nach dem `DETACH` verschwunden sein muss |
| `KEEP_CLIENT` | – | `1`: Client-Pod nach dem Lauf behalten |

## Konsistenz der Fixtures

```bash
go test ./test/e2e/
```

Die Go-Tests dieses Pakets ([fixtures_test.go](fixtures_test.go)) laufen ohne Docker und gehören zu `go test ./...`.
Sie prüfen die Compose-Fixtures mit dem Code des Gateways:

* Beide Gateway-Konfigurationen laden mit `config.Load` (unbekannte Felder sind Fehler) und unterscheiden sich nur im
  Session- und Engine-Timing.
* `users.yaml` akzeptiert die Tokens aus `e2e.env` über den echten `static`-Authenticator, weist den falschen Token
  ab und enthält keine Klartext-Tokens. `admin.token` und die Engine-Tokens passen zu `e2e.env`.
* Die Handshakes von `run.sh` (Token plus Optionen) laufen durch Authentifizierung und Policy: a, c, e und f landen
  auf dem Engine-Key der erwarteten statischen Engine, b wird mit „not a member“ abgelehnt, d ist nicht
  authentifiziert. Dabei wird auch geprüft, dass `run.sh` genau diese Tokens sendet.
* `docker-compose.yaml` setzt für die Engines nur Variablen des Engine-Container-Vertrags, das Init-SQL legt `whoami`
  an, die Gateways mounten die richtigen Dateien, und die URLs der statischen Engines zeigen auf die Services.

Wer einen Token in `e2e.env` ändert, muss seinen Hash in `users.yaml` (`quackmux hash-token TOKEN`) beziehungsweise
den Wert in `config/admin.token` oder `config/gateway*.yaml` nachziehen. Die Tests zeigen, was nicht passt.

## Fehlersuche

* **Compose:** Mit `E2E_KEEP=1` laufen die Container nach dem Test weiter:
  `docker compose -p quackmux-e2e --env-file test/e2e/compose/e2e.env -f test/e2e/compose/docker-compose.yaml logs gateway`.
  Die Gateways loggen auf Level `debug` mit Audit-Log, also jede Query.
* **kind:** `kubectl --context kind-quackmux -n quackmux logs deploy/quackmux`, Engine-Pods mit
  `kubectl --context kind-quackmux -n quackmux get pods -l app.kubernetes.io/component=engine`. Bei einem Fehler gibt
  `test.sh` Pods, Events und das Gateway-Log selbst aus.
* **`LOAD quack` scheitert:** Das Extension-Verzeichnis des Images passt nicht zu `E2E_EXTENSION_DIR`.
* **SELinux (Fedora, RHEL):** Bind-Mounts der Konfiguration brauchen eventuell eine Relabel-Option (`:z`).
* **Wie die Skripte mit DuckDB sprechen:** Kurze Szenarien geben der CLI ein SQL-Skript mit `-bail` auf stdin.
  Szenarien mit lange offenem Attachment schreiben über eine FIFO nach und nach in die CLI. Die CLI leert ihre
  Ausgabe, bevor sie die nächste Zeile liest; die Skripte warten deshalb auf Marker wie `SELECT 'released=ok'` und
  beenden die CLI mit `.quit`. Die gemeinsamen Hilfsfunktionen stehen in [lib/common.sh](lib/common.sh).
