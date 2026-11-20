# Engine-Image `quackmux/duckdb-engine`

Aus diesem Image startet quackmux seine Engines. Es enthält die DuckDB-CLI 2.0.0 mit vorinstallierten Extensions
(`quack`, `httpfs`), `tini` als PID 1 und den Entrypoint `quackmux-engine`. Der Entrypoint setzt den
Engine-Container-Vertrag aus [`internal/engine/initsql.go`](../../internal/engine/initsql.go) um: Er öffnet die
Datenbank, führt das Init-SQL aus und startet `quack_serve` auf `0.0.0.0:9494` über Plain HTTP. Danach läuft er im
Vordergrund, bis ein `SIGTERM` kommt. Das Image funktioniert auch ohne Gateway als einzelner Quack-Server.

| Pfad | Inhalt |
|---|---|
| `/usr/local/bin/duckdb` | DuckDB-CLI (offizielles Release-Binary `duckdb-cli-linux-<arch>.tar.gz`, glibc) |
| `/opt/duckdb/extensions` | vorinstallierte Extensions, gehört `root` und ist für die Engine nur lesbar |
| `/usr/local/bin/quackmux-engine` | Entrypoint ([`entrypoint.sh`](entrypoint.sh)) |
| `/usr/local/bin/quackmux-engine-selftest` | Smoke-Test des Images ([`selftest.sh`](selftest.sh)) |
| `/data` | leerer Mountpoint für Datenbankdateien, gehört dem Engine-Benutzer |

Der Prozess läuft als Benutzer `duckdb` (UID/GID 1000) mit `HOME=/tmp` und Arbeitsverzeichnis `/tmp`. Port: 9494.
Basis ist `debian:bookworm-slim`; das offizielle `duckdb/duckdb`-Image ist distroless und hat keine Shell.

## Umgebungsvariablen

| Variable | Default | Bedeutung |
|---|---|---|
| `QUACK_TOKEN` | – (Pflicht) | Token, das Clients vorlegen müssen, normalerweise das Gateway. Mindestens 16 Zeichen. Es erscheint weder in Prozessargumenten noch im Log. |
| `QUACKMUX_DATABASE` | `:memory:` | Datenbank, die DuckDB öffnet: `:memory:` oder ein Dateipfad, z. B. `/data/engine.duckdb`. |
| `QUACKMUX_INIT_SQL` | – | SQL, das vor dem Start des Servers läuft (`ATTACH`, `CREATE SECRET`, `SET` …). Das Gateway übergibt hier `settings` und `initSQL` des Profils. |
| `QUACKMUX_INIT_SQL_FILE` | – | Datei mit weiterem Init-SQL. Sie läuft nach `QUACKMUX_INIT_SQL`, z. B. aus einer ConfigMap oder einem Secret. |
| `QUACKMUX_PORT` | `9494` | Port des Quack-Servers. |
| `QUACKMUX_BIND` | `0.0.0.0` | Adresse, an die der Server bindet. |
| `QUACKMUX_EXTENSION_DIR` | `/opt/duckdb/extensions` | Verzeichnis, aus dem DuckDB Extensions lädt. |
| `QUACKMUX_MAX_CONNECTIONS` | DuckDB-Default (1024) | `quack_server_max_connections`: gleichzeitige TCP-Verbindungen. Der Server nutzt einen Thread pro Verbindung. |
| `QUACKMUX_KEEP_ALIVE_TIMEOUT` | DuckDB-Default (300) | `quack_server_keep_alive_timeout` in Sekunden. Der Wert sollte über `server.upstream.idleConnTimeout` des Gateways liegen (Default 90 s), damit das Gateway inaktive Verbindungen zuerst schließt. |
| `QUACKMUX_STOP_TIMEOUT` | `20` | So viele Sekunden (`20` oder `20s`) wartet der Entrypoint beim Stoppen auf DuckDB, danach folgt `SIGKILL`. Der Wert muss unter der Grace Period des Pods liegen (`provider.kubernetes.deleteGracePeriod`, Default 30 s). |
| `QUACKMUX_DUCKDB_BIN` | `duckdb` | Pfad der DuckDB-CLI, für eigene Images. |
| `TMPDIR` | `/tmp` | Hier legt der Entrypoint sein privates Verzeichnis an. Es muss beschreibbar sein. |
| `QUACKMUX_ENGINE_ID`, `QUACKMUX_SHARE_LEVEL`, `QUACKMUX_OWNER`, `QUACKMUX_SUBDOMAIN`, `QUACKMUX_PROFILE` | – | Setzt das Gateway. Sie erscheinen nur in der Logzeile. |

## Start

1. Der Entrypoint prüft die Variablen. Bei einem Fehler endet er mit Exit-Code 78.
2. Er legt ein privates Verzeichnis (`mktemp -d`, Modus 0700) mit einer FIFO an und hält sie auf fd 3 read-write
   offen. Dann startet er `duckdb -init /dev/null -bail <Datenbank>` mit der FIFO als stdin. Das Init-SQL kopiert er
   in private Dateien (Modus 0600).
3. Über die FIFO bekommt DuckDB:

   ```sql
   SET extension_directories = ['/opt/duckdb/extensions'];
   SET autoinstall_known_extensions = false;
   SET autoload_known_extensions = true;
   LOAD quack;
   SET GLOBAL quack_server_max_connections = …;     -- nur mit QUACKMUX_MAX_CONNECTIONS
   SET GLOBAL quack_server_keep_alive_timeout = …;  -- nur mit QUACKMUX_KEEP_ALIVE_TIMEOUT
   .read <Kopie von QUACKMUX_INIT_SQL>
   .read <Kopie von QUACKMUX_INIT_SQL_FILE>
   .output /dev/null
   CALL quack_serve('quack:0.0.0.0:9494', token = getenv('QUACK_TOKEN'),
                    allow_other_hostname = true, disable_ssl = true);
   .output <Marker-Datei>
   ```

4. Legt DuckDB die Marker-Datei an, ist alles fehlerfrei gelaufen, denn `-bail` bricht beim ersten Fehler ab. Der
   Entrypoint loggt dann `quackmux-engine: serving on quack:0.0.0.0:9494 over plain HTTP (…)` und löscht das
   Verzeichnis. Die FIFO bleibt über fd 3 offen. Ab jetzt beantwortet der Server `GET /` ohne Authentifizierung. Das
   nutzt die Readiness-Probe.
5. Beendet sich DuckDB vorher, etwa wegen eines Fehlers im Init-SQL, meldet der Entrypoint das und endet mit dem
   Exit-Code von DuckDB, mindestens aber mit 1.

Warum eine FIFO: Die DuckDB-CLI beendet sich beim EOF auf stdin, und Kubernetes liefert ohne `stdin: true` sofort EOF.
`duckdb -serve` ist kein Daemon-Modus und kann nicht an `0.0.0.0` binden. Über die FIFO landen keine Statements in den
Prozessargumenten. Das Token liest DuckDB selbst per `getenv('QUACK_TOKEN')`. Die Ergebniszeile von `quack_serve`,
die das Token enthält, geht nach `/dev/null`.

### Hinweise zum Init-SQL

* Das Init-SQL läuft als Skript (`.read`). Mehrzeilige Statements, Kommentare, beliebige Quotes und Dot-Commands sind
  erlaubt. Der erste Fehler bricht den Start ab.
* Es läuft auf der Verbindung des Entrypoints. Datenbanken (`ATTACH`), Secrets, Tabellen und globale Einstellungen
  sehen alle Client-Sessions. `USE` und Einstellungen mit Session-Scope (z. B. `search_path`) gelten dagegen nur für
  diese eine Verbindung. Für Clients braucht es `SET GLOBAL …`.
* Profil-`settings` werden zu `SET name = 'wert';`. Die Quack-Server-Settings brauchen `SET GLOBAL` vor
  `quack_serve`. Dafür gibt es `QUACKMUX_MAX_CONNECTIONS` und `QUACKMUX_KEEP_ALIVE_TIMEOUT`, alternativ
  `SET GLOBAL …` im Init-SQL. YAML-Werte in `settings` müssen Strings sein (`threads: "4"`).
* Extensions lassen sich zur Laufzeit nicht installieren: Das Verzeichnis ist schreibgeschützt und Autoinstall ist
  aus. Vorinstallierte Extensions lädt `LOAD` oder Autoload. Weitere Extensions kommen ins Image (siehe unten).
* `enable_external_access = false` darf nicht im Init-SQL stehen. Das Token wird erst danach per `getenv()`
  gelesen, und DuckDB sperrt `getenv()` ohne External Access.

## Stoppen

Auf `SIGTERM`, `SIGINT` oder `SIGHUP` schickt der Entrypoint:

```sql
.bail off
CALL quack_stop('quack:0.0.0.0:9494');
FORCE CHECKPOINT;   -- nur bei einer Datenbankdatei
.quit
```

`FORCE CHECKPOINT` bricht Transaktionen ab, die Clients offen gelassen haben, statt am Checkpoint zu scheitern. Der
Entrypoint wartet höchstens `QUACKMUX_STOP_TIMEOUT` Sekunden, dann beendet er DuckDB mit `SIGKILL`. Ein zweites Signal
beendet DuckDB sofort. Kommt das Signal noch während des Starts, unterbricht der Entrypoint das laufende
Init-Statement mit `SIGINT`.

| Exit-Code | Bedeutung |
|---|---|
| 0 | nach einem Stop-Signal sauber beendet |
| 1 | DuckDB-Fehler, z. B. im Init-SQL; auch wenn sich DuckDB ohne Stop-Signal beendet hat |
| 78 | Konfigurationsfehler, z. B. `QUACK_TOKEN` fehlt |
| 137 | DuckDB per `SIGKILL` beendet (Stop-Timeout oder zweites Signal) |
| sonst | Exit-Code von DuckDB |

## Bauen

```sh
docker build -t quackmux/duckdb-engine:2.0.0 images/engine
make docker-engine        # dasselbe, Variablen ENGINE_IMAGE, ENGINE_TAG, DUCKDB_VERSION, PLATFORM
```

Der Build-Kontext ist `images/engine`. Beim Bauen lädt Docker das Release von GitHub und die Extensions von
`extensions.duckdb.org`. Am Ende startet der Build `quackmux-engine-selftest` im fertigen Image als UID 1000: Start,
`GET /`, kein Token in Ausgabe und Prozessargumenten, `SIGTERM`, Exit-Code 0 und ein Init-SQL-Ergebnis in einer
Datenbankdatei. Schlägt der Test fehl, bricht der Build mit dem Log der Engine ab.

| Build-Argument | Default | Bedeutung |
|---|---|---|
| `DUCKDB_VERSION` | `2.0.0` | DuckDB-Release (`v` davor ist erlaubt) |
| `DUCKDB_EXTENSIONS` | `quack httpfs` | Extensions, die vorinstalliert und testweise geladen werden; `name` oder `name@repository` |
| `DUCKDB_SHA256` | – | SHA-256 des Release-Archivs. Gesetzt prüft der Build mit `sha256sum -c`. Gilt für jede Architektur. |
| `DUCKDB_SHA256_AMD64`, `DUCKDB_SHA256_ARM64` | – | Prüfsummen je Architektur, für Multi-Platform-Builds |
| `DUCKDB_RELEASE_URL` | `https://github.com/duckdb/duckdb/releases/download` | Basis-URL der Downloads, z. B. ein Mirror |
| `DUCKDB_EXTENSION_REPOSITORY` | – | Mirror des Extension-Repositorys (`custom_extension_repository`) |
| `BASE_IMAGE` | `debian:bookworm-slim` | Basis beider Stages, z. B. per Digest gepinnt |
| `ENGINE_SELFTEST` | `true` | `false` überspringt den Smoke-Test |
| `TARGETARCH` | von BuildKit | `amd64` oder `arm64`; ohne BuildKit die Architektur des Builders |

Mit geprüfter Prüfsumme:

```sh
curl -fsSL https://github.com/duckdb/duckdb/releases/download/v2.0.0/duckdb-cli-linux-amd64.tar.gz | sha256sum
docker build --build-arg DUCKDB_SHA256=<hash> -t quackmux/duckdb-engine:2.0.0 images/engine
```

Multi-Platform:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --build-arg DUCKDB_SHA256_AMD64=<hash> --build-arg DUCKDB_SHA256_ARM64=<hash> \
  -t registry.example.com/quackmux/duckdb-engine:2.0.0 --push images/engine
```

Beide Stages laufen auf der Zielplattform, beim Cross-Build also unter QEMU-Emulation (binfmt muss eingerichtet
sein). Grund: `INSTALL` lädt Extensions für die Plattform des laufenden DuckDB-Binarys.

Den Smoke-Test kann man auch später ausführen, etwa für ein abgeleitetes Image:

```sh
docker run --rm --entrypoint quackmux-engine-selftest quackmux/duckdb-engine:2.0.0
```

## Weitere Extensions

Per Build-Argument, z. B. für DuckLake mit PostgreSQL-Katalog und AWS-Credentials:

```sh
docker build --build-arg DUCKDB_EXTENSIONS="quack httpfs aws ducklake postgres_scanner" \
  -t registry.example.com/quackmux/duckdb-engine:2.0.0-lake images/engine
```

Community-Extensions mit `name@community` (z. B. `h3@community`), eigene Repositories mit `name@https://…`.

Oder als abgeleitetes Image:

```dockerfile
FROM quackmux/duckdb-engine:2.0.0
USER 0
RUN HOME=/root duckdb -init /dev/null -bail -c "SET extension_directories = ['/opt/duckdb/extensions']; \
      INSTALL aws; INSTALL ducklake; INSTALL postgres_scanner; LOAD aws; LOAD ducklake; LOAD postgres_scanner;" \
 && chmod -R a+rX,go-w /opt/duckdb
USER 1000:1000
```

Im Profil des Gateways:

```yaml
profiles:
  lake:
    image: registry.example.com/quackmux/duckdb-engine:2.0.0-lake
    settings:
      threads: "4"
      memory_limit: 8GB
    initSQL: |
      CREATE SECRET (TYPE s3, PROVIDER credential_chain);
      ATTACH 'ducklake:postgres:dbname=catalog host=postgres' AS lake (DATA_PATH 's3://bucket/lake/');
```

## Lokal testen

```sh
docker run --rm --name quack-engine -p 9494:9494 \
  -e QUACK_TOKEN=lokales-test-token-0123456789 \
  -e QUACKMUX_INIT_SQL="CREATE TABLE answers AS SELECT 42 AS answer;" \
  quackmux/duckdb-engine:2.0.0
```

Das Banner kommt ohne Token:

```sh
curl http://localhost:9494/
```

Ein DuckDB-2.0-Client auf dem Host:

```sql
INSTALL quack;
LOAD quack;
CREATE SECRET (TYPE quack, TOKEN 'lokales-test-token-0123456789', SCOPE 'quack:localhost:9494');
ATTACH 'quack:localhost:9494' AS engine;
FROM engine.query('FROM answers');
DETACH engine;
```

`localhost` spricht der Client per HTTP an. Für andere Hosts nimmt er HTTPS an, die Engine spricht aber Plain HTTP.
Dort heißt es `ATTACH 'quack:<host>:9494' AS engine (DISABLE_SSL true)`. Im Betrieb steht das Gateway davor.

Mit einer Datenbankdatei auf einem Volume:

```sh
docker run --rm --name quack-engine -p 9494:9494 -v quack-data:/data \
  -e QUACK_TOKEN=lokales-test-token-0123456789 -e QUACKMUX_DATABASE=/data/engine.duckdb \
  quackmux/duckdb-engine:2.0.0
```

`docker stop quack-engine` stoppt den Server, checkpointet die Datei und beendet DuckDB. Das zeigt das Log.

## In Kubernetes

Der Kubernetes-Provider des Gateways setzt die Umgebungsvariablen, holt das Token aus einem Secret und mountet ein
`emptyDir` auf `/tmp`. Das Root-Dateisystem ist read-only, nur `/tmp` muss beschreibbar sein. Die Readiness-Probe ist
`GET /` auf Port 9494. Der numerische `USER 1000:1000` erfüllt `runAsNonRoot`. Für persistente Engines gehören ein
PVC auf `/data` (mit `fsGroup: 1000`) ins `podTemplate` und `database: /data/engine.duckdb` ins Profil.

## Sicherheit

* Das Token steht nie in Prozessargumenten oder Logs. Das Init-SQL liegt nur in privaten Dateien. Sie werden
  gelöscht, sobald der Server läuft. `QUACKMUX_INIT_SQL` entfernt der Entrypoint aus der Umgebung von DuckDB.
* Clients einer Engine führen beliebiges SQL als Engine-Benutzer aus. Mit External Access, dem DuckDB-Default,
  können sie Dateien und Umgebungsvariablen des Engine-Prozesses lesen, auch `QUACK_TOKEN`. Deshalb darf nur das
  Gateway Port 9494 erreichen (NetworkPolicy). In geteilten Engines (`GROUP`, `SERVER`) gehören ins Init-SQL nur
  Secrets, die alle Benutzer der Engine verwenden dürfen.
* Das Extension-Verzeichnis ist schreibgeschützt und Autoinstall ist aus. Engines brauchen deshalb keinen
  Internetzugang.
