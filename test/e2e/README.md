# End-to-End-Tests

Diese Tests prüfen quackmux mit **echten DuckDB-2.0-Clients und -Engines**. Es gibt zwei Umgebungen:

| Umgebung | Verzeichnis | Was läuft | Dauer |
|---|---|---|---|
| Docker Compose | [compose](compose) | Gateway mit Provider `static` vor zwei Engine-Containern, Auth `static` | ca. 2,5 min (plus Build) |
| kind | [kind](kind) | Overlay `deploy/kubernetes/overlays/dev`: Das Gateway startet Engine-Pods selbst, auch mit Volume pro Benutzer | einige Minuten |

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
| `engine-analytics` | Engine-Image | Dasselbe für Share-Level `GROUP` der Gruppe `analytics` (`engine = 'analytics'`), gehärtet wie eine geteilte Engine: Autorisierungs-Makro des Images in der Variante `quackmux_authorize_listing` im Init-SQL, `enable_external_access = false` und `lock_configuration = true` im Post-Start-SQL. |
| `gateway` | Gateway-Image | quackmux mit [config/gateway.yaml](compose/config/gateway.yaml): Provider `static` auf beide Engines, Auth `static` mit [config/users.yaml](compose/config/users.yaml), Admin-API mit Bearer-Token. Schließt nach einem Neustart verwaiste Verbindungen (`engine.closeOrphanedConnections: true`); mit `session.backendHeartbeatTimeout: 60s` prüft es sie schon nach 75 s (Szenario k). |
| `gateway-idle` | Gateway-Image | Wie `gateway`, aber [config/gateway-idle.yaml](compose/config/gateway-idle.yaml) mit `session.idleTimeout: 15s` und Prüfintervallen von 1 s, ohne das Schließen verwaister Verbindungen (es startet im Lauf nie neu). |
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
| h | bob (Query-Zeitlimit 5 s per `overrides`): eine kurze Query, dann drei lange: `remote.query('SELECT count(*) … FROM range(1000000000000)')` (die Engine zählt 10¹² Zeilen), ein Join von `remote.query(… range(5000000000))` mit einer lokalen Tabelle (der Client holt die Zeilen) und `CREATE TABLE remote.h_upload AS …` aus einer lokalen Query (der Client lädt hoch), nach jeder eine weitere Query | Die kurze Query bleibt unberührt. Jede lange bricht das Gateway nach 5 s auf der Engine ab (CANCEL): während die Engine rechnet, während der Client FETCHes schickt und während er hochlädt. bob bekommt jedes Mal „ran longer than its time limit of 5s“, die nächste Query der Session läuft, und das abgebrochene `CREATE TABLE … AS` hinterlässt keine Tabelle. `quackmux_query_timeouts_total{result="cancelled"}` zählt alle drei, `quackmux_requests_total{outcome="query_timeout"}` auch für `FETCH_REQUEST` und `SEND_DATA_REQUEST`. |
| j | alice, `share_level=group&group=analytics` auf der gehärteten Engine: `ATTACH`, `SHOW ALL TABLES`, Upload einer lokalen Tabelle, temporäre Tabelle, `FROM _`. Danach laufen die Fälle des Autorisierungs-Makros ([images/engine/authorize-cases.tsv](../../images/engine/authorize-cases.tsv)) mit `quackmux-authorize-test` aus dem Client-Image durch das Gateway, zuletzt fragt ein Client `whoami` ab. | Das Makro lässt die Katalog-Abfragen des Clients, Queries, Uploads und Transaktionen durch und lehnt `quack_stop`, `getenv`, `COPY`, `EXPORT`/`IMPORT DATABASE` und die übrigen Fälle in jeder Schreibweise ab; Dateizugriff und `SET` scheitern an den Einstellungen des Post-Start-SQL. Das Engine-Token taucht nirgends auf, und die Engine läuft weiter. |
| k | Auf `engine-shared` und auf der gehärteten `engine-analytics` je drei Clients mit einer temporären Tabelle: zwei von alice über `gateway`, einer direkt an der Engine mit ihrem Token. Ein weiterer Client listet vorher und nachher mit `remote.query('SELECT connection_id FROM quack_active_connections()')` die Verbindungen der Engine. `gateway` wird gestoppt, je ein Client von alice wird ohne `DETACH` getötet (`docker kill`), dann startet `gateway` wieder. Wenn die Engine-Lease der Verbindungen abgelaufen ist (mit `gateway.yaml` nach 75 s), listet ein neuer Client erneut. | Die überlebenden Clients von alice werden übernommen. Den Heartbeat des Gateways beantwortet die Engine für die Verbindung des getöteten Clients mit abgelaufener Lease und entfernt sie: Sie wird nicht mehr gelistet, `quackmux_orphaned_connections_closed_total` steigt auf 2. Die Verbindungen der überlebenden und der direkt angehängten Clients bleiben, mit ihren temporären Tabellen, vor und nach dem Aufräumen. Die gehärtete Engine lässt das Listing mit `quackmux_authorize_listing` durch. |
| i | Logs beider Gateways | Kein Request-Handler ist mit einem Panic abgestürzt. |

### Variablen

| Variable | Default | Bedeutung |
|---|---|---|
| `E2E_SKIP_BUILD` | – | `1`: vorhandene Images verwenden, nicht bauen |
| `DOCKER_BUILD_FLAGS` | – | zusätzliche Flags für beide `docker build`, durch Leerzeichen getrennt wie im Makefile, z. B. `--network host --build-arg GOPROXY=…` hinter einem Proxy |
| `E2E_KEEP` | – | `1`: Container nach dem Lauf weiterlaufen lassen (zum Untersuchen) |
| `E2E_ENGINE_IMAGE` | `quackmux/duckdb-engine:2.0.0` | Engine- und Client-Image |
| `E2E_GATEWAY_IMAGE` | `quackmux/quackmux:e2e` | Gateway-Image |
| `E2E_EXTENSION_DIR` | `/opt/duckdb/extensions` | Extension-Verzeichnis im Engine-Image; leer: DuckDB-Default |
| `E2E_QUACK_PORT`, `E2E_ADMIN_PORT` | 19494, 19495 | Host-Ports von `gateway` |
| `E2E_IDLE_QUACK_PORT`, `E2E_IDLE_ADMIN_PORT` | 19594, 19595 | Host-Ports von `gateway-idle` |
| `E2E_PROJECT` | `quackmux-e2e` | Compose-Projektname; mit eigenem Namen und eigenen Ports kann ein zweiter Lauf parallel laufen |
| `NO_COLOR` | – | gesetzt: Ausgabe ohne Farben |

### Von Hand ausprobieren

```bash
cd test/e2e/compose
E2E_KEEP=1 bash run.sh        # oder: docker compose --env-file e2e.env up -d (nach dem Build)
curl -H "Authorization: Bearer $(cat config/admin.token)" http://127.0.0.1:19495/api/v1/sessions
docker compose -p quackmux-e2e --env-file e2e.env run --rm -T --no-deps client <<'SQL'
SET extension_directories = ['/opt/duckdb/extensions'];
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
bash test/e2e/kind/test.sh       # Szenarien k1–k8 (beliebig oft wiederholbar, etwa 6 min)
bash test/e2e/kind/ha.sh         # Hochverfügbarkeit: Szenarien h1–h6 mit overlays/ha
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
`DELETE /api/v1/engines/{id}`. Die Volumes seiner Benutzer löscht er am Ende über `DELETE /api/v1/volumes/{name}`.

| | Ablauf | Was es beweist |
|---|---|---|
| k1 | Zwei Sessions mit `share_level=connection`, gleichzeitig offen; danach `DETACH` der ersten | Jedes `ATTACH` bekommt einen eigenen Engine-Pod (`kubectl get pods -l app.kubernetes.io/component=engine` zeigt zwei neue Pods). Nach dem `DETACH` verschwindet der Pod der ersten Session binnen 30 s, der zweite läuft weiter. Das prüft `engine.idleTimeout.CONNECTION = 0`, das Löschen durch den Provider und das Stoppen der Engine bei SIGTERM. |
| k2 | Zwei Sessions desselben Benutzers mit `share_level=user`; die erste legt eine Tabelle an, die zweite liest sie | Beide Sessions teilen sich genau einen Engine-Pod und dessen Datenbank. |
| k3 | Der Engine-Pod einer `USER`-Session wird gelöscht. Danach eine Query auf demselben Attachment, dann `DETACH` und erneutes `ATTACH` | Das Gateway erkennt den Verlust über den Informer und schließt die Session. Die nächste Query scheitert mit „session closed“ statt zu hängen. Ein neues `ATTACH` startet einen neuen Pod und funktioniert. |
| k4 | Eine offene Session, dann `kubectl rollout restart` des Gateways, danach eine Query auf demselben Attachment | Das neue Gateway findet die Engine-Pods wieder (Discovery) und übernimmt die Session aus der versiegelten Connection-ID (Adoption): Die Query klappt, die Admin-API zeigt die Session mit `"adopted": true` auf derselben Engine. Das Dev-Overlay nutzt die Strategie `Recreate`; dass Heartbeats in der kurzen Pause scheitern, toleriert der Client. Scheitert die erste Query nach dem Neustart mit einem Transportfehler, wiederholt das Skript sie bis zu zweimal und meldet das als Warnung. |
| k5 | Eine Session mit `profile=persistent` legt eine Tabelle an. Die Admin-API draint ihre Engine, während die Session angehängt bleibt, und eine zweite Session desselben Benutzers attacht; dann detacht die erste. Danach wird der Engine-Pod gelöscht, und die zweite Session attacht neu. | Die Engine mountet einen eigenen Claim (`ReadWriteOncePod`, `Bound`, Annotation `quackmux.io/key` mit dem Schlüssel des Benutzers). Die zweite Session wartet, solange die gedrainte Engine eine Session hat: Ihr `ATTACH` bleibt offen, das Gateway-Log meldet `a new session waits for the previous engine of its key`. Die nächste Engine startet erst, wenn der alte Pod aus der API verschwunden ist, mit demselben Claim und der Tabelle. Auch nach dem Löschen des Pods ist die Tabelle da. Ein Watch auf `quackmux.io/key-hash` sieht nie zwei Pods. |
| k6 | Zwei Benutzer mit `profile=persistent`; der zweite fragt die Tabelle des ersten ab. Ein dritter Client wählt `subdomain=other`. | Jeder Benutzer hat seinen eigenen Claim und sieht nur seine Daten („does not exist“). Eine Subdomain außerhalb von `allowedSubdomains` lehnt das Gateway ab („does not allow subdomain“). |
| k7 | Das Profil `persistent` wird geändert (eine neue ConfigMap mit zusätzlichem Label, Rollout des Gateways). Eine zweite Session kommt hinzu, danach enden beide; dann attacht eine dritte. | Die veraltete Engine nimmt weiter Sessions an (`replacement pending`, zwei Sessions), statt neben einer Nachfolgerin zu laufen. Sie endet, sobald sie idle ist (nach `engine.recoveryGrace` des neu gestarteten Gateways, daher dauert k7 etwa fünf Minuten); erst dann startet die Nachfolgerin, mit demselben Claim und denselben Daten. Der Watch sieht nie zwei Pods des Schlüssels. Danach stellt das Skript die Konfiguration wieder her. |
| k8 | `GET /api/v1/volumes?owner=…`, `DELETE /api/v1/volumes/{name}` mit laufender Engine, mit einem Debug-Pod, der den Claim mountet, und ohne beides | Die Admin-API listet den Claim mit seiner Engine. Sie lehnt das Löschen ab (409), solange die Engine existiert und solange ein anderer Pod den Claim mountet (das Gateway fragt dafür die Kubernetes-API, nicht seine Registry), und löscht ihn danach (202). |

Anforderungen an das Overlay (das Dev-Overlay erfüllt sie): `auth.mode: none`, ein gemeinsamer Session-Key
(`server.sessionKeyFile` oder `QUACKMUX_SESSION_KEY`, sonst scheitert k4 mit „Invalid connection id“), ein Service mit
Port 9494, Platz für mindestens zwei gleichzeitige Engines (`limits.maxEngines`), auch für denselben Benutzer
(`limits.maxEnginesPerUser`), Pool-Größe 1 und ein kurzes `engine.checkInterval` (Default 5 s). Für k5–k8 ein Profil
`persistent` mit `allowedShareLevels: [USER]`, `allowedSubdomains: [default]` und `persistence`, die ConfigMap der
Gateway-Konfiguration im Volume `config` des Deployments (k7 tauscht sie aus) und eine Default-StorageClass, die
`ReadWriteOncePod` kann (in kind: `standard`, local-path). Ohne Default-StorageClass bricht `test.sh` gleich zu Beginn
ab.

| Variable | Default | Bedeutung |
|---|---|---|
| `KIND_CLUSTER` | `quackmux` | Name des kind-Clusters |
| `KUBE_CONTEXT` | `kind-$KIND_CLUSTER` | kubectl-Kontext |
| `OVERLAY_DIR` | `deploy/kubernetes/overlays/dev` | Kustomize-Overlay |
| `ENGINE_IMAGE`, `GATEWAY_IMAGE` | aus dem Overlay | Image-Namen |
| `QMX_NAMESPACE`, `QMX_DEPLOYMENT` | aus dem Overlay | Namespace und Deployment des Gateways |
| `QMX_SERVICE`, `QMX_SERVICE_PORT` | Service mit Port 9494 | Ziel der Clients |
| `SKIP_BUILD` | – | `1`: Images nicht bauen, nur laden (setup.sh) |
| `DOCKER_BUILD_FLAGS` | – | zusätzliche Flags für beide `docker build` (setup.sh), wie beim Compose-Test |
| `KIND_NODE_IMAGE` | kind-Default | Node-Image für `kind create cluster` |
| `KIND_CONFIG` | – | Cluster-Konfiguration für `kind create cluster --config`. Auf Hosts mit cgroup v1 startet das Kubelet ab Kubernetes 1.35 nur mit `failCgroupV1: false` (siehe Fehlersuche). |
| `ADMIN_TOKEN` | – | Bearer-Token, falls die Admin-API einen verlangt |
| `ADMIN_LOCAL_PORT` | 19695 | lokaler Port des Port-Forwards |
| `CLIENT_UID` | 1000 | Benutzer des Client-Pods (der Benutzer des Engine-Images) |
| `ATTACH_TIMEOUT` | 180 | Sekunden für ein `ATTACH` inklusive Start eines Engine-Pods |
| `POD_GONE_TIMEOUT` | 30 | Sekunden, bis der `CONNECTION`-Pod nach dem `DETACH` verschwunden sein muss |
| `KEEP_CLIENT` | – | `1`: Client-Pod nach dem Lauf behalten |

### Hochverfügbarkeit (ha.sh)

**ha.sh** prüft `deploy/kubernetes/overlays/ha` mit echten DuckDB-2.0-Clients im Cluster von setup.sh. Es geht vor
wie ein Betreiber nach [deploy/kubernetes/README.md](../../deploy/kubernetes/README.md):

1. Es erzeugt ein Overlay auf `overlays/ha` mit frischen Secrets (Session-Key, Admin-Token, `users.yaml` mit einem
   Token für den Testbenutzer) und dem lokal gebauten Gateway-Image. Das Overlay liegt in
   `test/e2e/kind/.work/ha-overlay` (von Git ignoriert) und wird danach gelöscht.
2. Es skaliert das laufende Gateway auf 0 und rollt das Overlay aus.
3. Am Ende stellt es das Dev-Overlay wieder her, damit test.sh wie gewohnt läuft (auch der Selektor des Service
   verliert dabei `quackmux.io/leader`, das prüft es), und löscht die ConfigMaps und Secrets, die nur das erzeugte
   Overlay verwendet hat: die mit Hash-Suffix aus dessen Render (HA-Konfiguration, frische Secrets), die das
   Dev-Overlay nicht enthält und die kein Deployment und kein Pod im Namespace nennt. Objekte ohne Suffix wie
   `quackmux-users` (beide Overlays schreiben sie) und alles, was das Dev-Overlay rendert oder nutzt, bleiben.

Zwei Clients desselben Benutzers bleiben über alle Szenarien angehängt (`share_level=user`, eine gemeinsame
Engine). Der erste nutzt den Default-Heartbeat-Timeout (60 s). Der zweite nutzt `HEARTBEAT_TIMEOUT 5`; seine
Session-Lease (5 s plus `session.heartbeatGrace` 15 s) ist kürzer, als h3 den Leader einfriert. Nach jedem
Failover müssen beide ihre Tabelle auf der Engine weiter lesen können. Eine Query, die während eines Failovers
unterwegs ist, darf scheitern (nie doppelt laufen); das Skript wiederholt sie bis zu zweimal und warnt.

| | Ablauf | Was es beweist |
|---|---|---|
| h1 | Overlay ausrollen | `kubectl rollout status` meldet Erfolg, das Deployment zeigt `UP-TO-DATE 2, AVAILABLE 2`. Beide Replikas sind ready; genau eine trägt `quackmux.io/leader=true` und die Löschkosten 100, hält die Lease `quackmux-default` und ist der einzige Endpunkt des Service. Der Follower besteht `/readyz` und meldet `quackmux_is_leader 0` und `quackmux_leader_label 0`. Ein Client, der sich direkt mit dem Follower verbindet, bekommt `not the leader` (gezählt als `outcome="not_leader"`). |
| h2 | `kubectl delete pod` des Leaders | Der alte Leader entfernt sein Label (laut seinem Log vor der Freigabe der Lease) und gibt die Lease frei; der wartende Follower führt nach höchstens `HA_HANDOVER_SECONDS` (gemessen 2–3 s), der Service ist höchstens `HA_GAP_SECONDS` ohne bereiten Endpunkt (gemessen 0,6–2 s), die Lease wechselt einmal. Laut seinem Log schließt der alte Leader seinen Quack-Listener frühestens `HA_DEREGISTRATION_SECONDS` nach dem Entfernen des Labels. Der Ersatz-Pod bewirbt sich erst nach `3 × ha.retryPeriod`. Der neue Leader adoptiert beide Sessions auf derselben Engine. |
| h3 | Den Gateway-Prozess des Leaders auf dem kind-Node mit `SIGSTOP` einfrieren, nach `HA_FREEZE_SECONDS` mit `SIGCONT` wecken | Der Follower übernimmt nach Ablauf der Lease (`ha.leaseDuration`, gemessen 18–19 s nach dem Einfrieren), ist danach der einzige Endpunkt und entfernt das Label vom eingefrorenen Pod. Der aufgeweckte Prozess merkt, dass er die Lease verloren hat, beendet sich und startet als bereiter Follower ohne Label neu. Dabei schließt er keine Session und gibt die Lease des neuen Leaders nicht frei. Danach arbeiten beide Clients weiter. |
| h4 | `kubectl rollout restart` | `kubectl rollout status` meldet ohne Eingriff Erfolg (gemessen nach 14–17 s). Die Lease wechselt genau einmal, zu einem Pod mit neuem Template: Wegen der Löschkosten des Leaders entfernt Kubernetes den alten Follower zuerst. Der Service ist höchstens `HA_GAP_SECONDS` ohne bereiten Endpunkt (gemessen 0,5–3,1 s), und der alte Leader hält seinen Quack-Listener wie in h2 offen. Danach `UP-TO-DATE 2, AVAILABLE 2`, und die Clients arbeiten weiter. |
| h4b | Eine Konfiguration ausrollen, die die Lease in einen Namespace ohne Role des Gateways verlegt (`ha.namespace: default`), danach das Overlay wieder anwenden | Der neue Pod startet nicht (`ha: this replica could never lead: …`), `kubectl rollout status` meldet keinen Erfolg, und wegen `maxUnavailable: 0` bleiben beide alten Pods bereit (`3 1 2`). Der alte Leader bedient ohne Übergabe weiter, die Clients merken nichts. Das erneute `kubectl apply -k` rollt zurück, die Lease wechselt nicht. |
| h5 | Eviction-API wie `kubectl drain` | Der Leader darf gehen, solange der Follower ready ist, und übergibt wie beim Löschen (höchstens `HA_GAP_SECONDS` ohne Endpunkt, gemessen 0,6–3,3 s). Direkt danach lehnt das PodDisruptionBudget die Räumung des neuen Leaders ab, der jetzt die letzte bereite Replika ist. Ist der Ersatz-Pod ready, darf der Follower gehen. |
| Ende | `DETACH` beider Clients | Beide Clients lösen sich sauber, kein `DETACH` hängt (etwa an einem Heartbeat, der im Verbindungsaufbau zu einem beendeten Pod hängt). |

h3 braucht `docker exec` auf dem kind-Node (`crictl inspect` liefert die PID des Gateway-Containers). Wer
Fehler untersuchen will, lässt das HA-Deployment mit `KEEP_HA=1` stehen; das erzeugte Overlay mit seinen Secrets
bleibt dann ebenfalls liegen.

**h6, persistente Schlüssel mit HA:** ha.sh fügt der Konfiguration des erzeugten Overlays das Profil `persistent`
hinzu (ein Volume pro Benutzer, Subdomains `default` und `late`); hat das Overlay keine `config.yaml` mit
`profiles:`, entfällt h6 mit einer Warnung. Eine Session des Profils legt eine Tabelle an, dann friert das Skript
den Leader ein wie in h3. Währenddessen verbindet sich ein zweiter Client direkt mit dem eingefrorenen Pod, für die
Subdomain `late` (ein Schlüssel ohne Engine), und nach der Übernahme attacht ein dritter über den Service. Geprüft
wird:

* Der neue Leader übernimmt die Engine und gibt der neuen Session des Schlüssels dieselbe; sie sieht die Tabelle.
  Ein Watch auf `quackmux.io/key-hash` sieht nie zwei Pods des Schlüssels.
* Der aufgeweckte Leader beantwortet das wartende `ATTACH` mit einem Fehler und startet keine Engine für `late`:
  Ohne frische Lease startet er keine Engine eines persistenten Schlüssels.
* Auf dem Follower antworten `GET` und `DELETE /api/v1/volumes` mit 503.
* Die Session von vor dem Einfrieren arbeitet weiter. Am Ende löscht das Skript Engine und Claim über die
  Admin-API des Leaders.

| Variable | Default | Bedeutung |
|---|---|---|
| `HA_OVERLAY` | `../../../../../deploy/kubernetes/overlays/ha` | das geprüfte Overlay, relativ zum erzeugten |
| `HA_LEASE` | `quackmux-default` | Name der Lease |
| `HA_HANDOVER_SECONDS` | 12 | Höchstdauer der Übergabe in h2 (eine ablaufende statt freigegebene Lease bräuchte mindestens 15 s) |
| `HA_GAP_SECONDS` | 8 | Höchstdauer ohne bereiten Endpunkt des Service bei einer geordneten Übergabe (h2, h4, h5); der Follower fragt alle `ha.retryPeriod` plus Jitter, also bis zu 4,4 s |
| `HA_DEREGISTRATION_SECONDS` | 3 | `ha.deregistrationDelay` des geprüften Overlays: so lange muss ein Leader, der herunterfährt, seinen Quack-Listener nach dem Entfernen des Labels offen halten (h2, h4) |
| `HA_FREEZE_SECONDS` | 45 | Dauer des Einfrierens in h3: länger als die Session-Lease des zweiten Clients, kürzer als die Liveness-Probe des Gateways toleriert (etwa 60 s) |
| `ADMIN_LOCAL_PORT` | 19696 | lokaler Port des Port-Forwards auf die Admin-API eines bestimmten Pods |
| `KEEP_HA` | – | `1`: HA-Deployment und erzeugtes Overlay nach dem Lauf behalten |

## Konsistenz der Fixtures

```bash
go test ./test/e2e/
```

Die Go-Tests dieses Pakets ([fixtures_test.go](fixtures_test.go)) laufen ohne Docker und gehören zu `go test ./...`.
Sie prüfen die Compose-Fixtures mit dem Code des Gateways:

* Beide Gateway-Konfigurationen laden mit `config.Load` (unbekannte Felder sind Fehler) und unterscheiden sich nur im
  Session- und Engine-Timing und darin, dass nur `gateway` verwaiste Verbindungen schließt (Szenario k: eine
  Engine-Lease von mindestens einer Minute, damit die überlebenden Clients ihre Verbindungen wiederfinden, und eine
  Frist von höchstens zwei Minuten).
* Das Autorisierungs-Makro des Engine-Images lehnt die Anfrage ab, mit der das Gateway die Verbindungen einer Engine
  listet, seine Variante `quackmux_authorize_listing` lässt genau sie durch; die Fälle prüfen beides, und die
  gehärtete Engine der Compose-Umgebung nutzt die Variante ([authorize_test.go](authorize_test.go)).
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
* **kind: Control-Plane startet nicht, Kubelet meldet „configured to not run on a host using cgroup v1“:** Ab
  Kubernetes 1.35 verweigert das Kubelet cgroup v1. Auf solchen Hosts (ältere Distributionen, manche CI-Runner)
  hilft eine Cluster-Konfiguration, die `KIND_CONFIG` an `setup.sh` übergibt:

  ```yaml
  kind: Cluster
  apiVersion: kind.x-k8s.io/v1alpha4
  kubeadmConfigPatches:
    - |
      kind: KubeletConfiguration
      failCgroupV1: false
  ```
* **kind: Pods bleiben in `ContainerCreating`, containerd meldet „failed to update /proc/self/oom_score_adj:
  Permission denied“:** Der Host erlaubt kein negatives `oom_score_adj` (fehlende Capability `CAP_SYS_RESOURCE`,
  etwa in manchen Sandboxen). Dann gehören in dieselbe Konfiguration zusätzlich:

  ```yaml
  containerdConfigPatches:
    - |-
      [plugins."io.containerd.cri.v1.runtime"]
        restrict_oom_score_adj = true
  ```
* **SELinux (Fedora, RHEL):** Bind-Mounts der Konfiguration brauchen eventuell eine Relabel-Option (`:z`).
* **Wie die Skripte mit DuckDB sprechen:** Kurze Szenarien geben der CLI ein SQL-Skript mit `-bail` auf stdin.
  Szenarien mit lange offenem Attachment schreiben über eine FIFO nach und nach in die CLI. Die CLI leert ihre
  Ausgabe, bevor sie die nächste Zeile liest; die Skripte warten deshalb auf Marker wie `SELECT 'released=ok'` und
  beenden die CLI mit `.quit`. Die gemeinsamen Hilfsfunktionen stehen in [lib/common.sh](lib/common.sh).
