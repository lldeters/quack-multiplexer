# quackmux – ein Kyuubi für DuckDB Quack

**quackmux** ist ein Gateway für [Quack](https://duckdb.org/docs/2.0/quack/overview), das
Client-Server-Protokoll von DuckDB 2.0. DuckDB-Clients verbinden sich wie mit einem normalen Quack-Server:

```sql
CREATE SECRET (TYPE quack, TOKEN 'mein-token', SCOPE 'quack:quackmux.example.com:9494');
ATTACH 'quack:quackmux.example.com:9494' AS lake (DISABLE_SSL true);
FROM lake.query('SELECT 42');
```

Das Gateway authentifiziert den Client und entscheidet anhand von Optionen, auf welcher **Engine** die
Verbindung landet. Eine Engine ist ein Kubernetes-Pod mit DuckDB und `quack_serve`. quackmux startet Engines
bei Bedarf, multiplext Sessions darauf und räumt nach Timeouts auf, genau wie Apache Kyuubi das für Spark tut.

| Share-Level | Verhalten |
|---|---|
| `CONNECTION` | eigener Pod pro `ATTACH`, wird beim `DETACH` beendet |
| `USER` *(Default)* | ein Pod pro Benutzer, von allen seinen Sessions geteilt |
| `GROUP` | ein Pod pro Gruppe |
| `SERVER` | ein Pod für alle |

Dazu kommen **Subdomains** (mehrere getrennte Engines je Owner), **Pools** (N Engines pro Key, RANDOM/POLLING),
ein **Warm-Pool** vorgestarteter Pods für schnelle `CONNECTION`-Sessions und **persistente Profile** mit einem
eigenen Volume pro Benutzer, Gruppe oder Subdomain (`profiles.<name>.persistence`, siehe
[deploy/kubernetes/README.md](deploy/kubernetes/README.md#persistente-daten)).

## Optionen setzen

Option über den Token. Das funktioniert mit jedem Client-Transport:

```sql
CREATE SECRET (TYPE quack, TOKEN 'mein-token?share_level=connection&profile=large',
               SCOPE 'quack:quackmux.example.com:9494');
```

Option als HTTP-Header (`EXTRA_HTTP_HEADERS` des Secrets). Das geht nicht zusammen mit `ssl_fingerprint`:

```sql
CREATE SECRET (TYPE quack, TOKEN 'mein-token', SCOPE 'quack:quackmux.example.com:9494',
               EXTRA_HTTP_HEADERS MAP {'X-Quackmux-Share-Level': 'server', 'X-Quackmux-Subdomain': 'bi'});
```

| Option | Header | Werte |
|---|---|---|
| `share_level` | `X-Quackmux-Share-Level` | `connection`, `user`, `group`, `server` |
| `subdomain` | `X-Quackmux-Subdomain` | `[a-z0-9-]`, max. 32 Zeichen (Default `default`) |
| `profile` | `X-Quackmux-Profile` | ein in der Konfiguration definiertes Engine-Profil |
| `group` | `X-Quackmux-Group` | Gruppe für `GROUP` (Default: erste Gruppe des Benutzers) |

Welche Kombinationen ein Benutzer wählen darf, regeln Profile (`allowedShareLevels`, `allowedUsers`,
`allowedGroups`) und `overrides` in der Konfiguration.

## Session-Management

| Mechanismus | Default | Wirkung |
|---|---|---|
| `session.idleTimeout` | 6 h | Session ohne Query wird geschlossen. Heartbeats zählen nicht. |
| Heartbeat-Lease | Client-Lease + 15 s | Stumme Clients werden erkannt, das Gateway schickt `DISCONNECT` an die Engine. Der Quack-Server selbst räumt solche Verbindungen nie auf. |
| `session.maxLifetime` | aus | Harte Obergrenze pro Session. |
| `limits.queryTimeout` | aus | Längste Laufzeit einer Query, das Abholen des Ergebnisses eingeschlossen (`COMMIT` und `ROLLBACK` ausgenommen). Danach bricht das Gateway sie auf der Engine ab, der Client bekommt einen Fehler, die Session bleibt. Profile und `overrides` ersetzen den Wert ([Design, 5.3](docs/design.md#53-query-zeitlimit)). |
| `engine.idleTimeout` | CONNECTION 0, sonst 30 min | Engine ohne Sessions wird beendet. |
| `engine.startupTimeout` | 180 s | Start-Budget für einen Pod. |
| `engine.maxLifetime` / `drainTimeout` | aus / 1 h | Alte Engines werden gedrained: keine neuen Sessions, alte laufen aus. |
| `engine.replaceOnSpecChange` | an | Ändert sich ein Profil, werden Engines rollierend ersetzt. |
| Neustart / Failover | – | Gateway-Connection-IDs sind versiegelte Routen (AES-GCM). Sessions überleben Neustarts und Leader-Wechsel, Engines werden wiedergefunden. |
| `engine.closeOrphanedConnections` | an (`static`: aus) | Nach einem Neustart räumt das Gateway die Verbindungen von Clients ab, die in der Downtime gestorben sind: Verbindungen ohne Session, deren Engine-Lease abgelaufen ist (nach `session.backendHeartbeatTimeout` plus `session.heartbeatGrace`, 1 h 15 s). |

## Schnellstart

```powershell
go build -o bin/quackmux.exe ./cmd/quackmux
```

```powershell
bin/quackmux.exe validate -config examples/config.local.yaml
```

Für die lokale Entwicklung startet der Provider `process` DuckDB-CLI-Prozesse statt Pods:

```powershell
bin/quackmux.exe serve -config examples/config.local.yaml
```

Kubernetes (Kustomize):

```powershell
kubectl apply -k deploy/kubernetes/base
```

Details zu Secrets, Images und HA stehen in [deploy/kubernetes/README.md](deploy/kubernetes/README.md).
Releases veröffentlichen Binaries und Images für amd64 und arm64 (`ghcr.io/lldeters/quackmux`,
`ghcr.io/lldeters/duckdb-engine`); CI und Release-Ablauf beschreibt [docs/releasing.md](docs/releasing.md).

## Aufbau

| Pfad | Inhalt |
|---|---|
| [cmd/quackmux](cmd/quackmux) | Binary (`serve`, `validate`, `token`, `hash-token`) |
| [internal/wire](internal/wire) | Quack-Wire-Codec (Header, Handshake, Fehler, Kontrollnachrichten) |
| [internal/proxy](internal/proxy) | Quack-Endpoint: Handshake, Routing, Header-Rewrite, Streaming/Puffern |
| [internal/session](internal/session) | Sessions, versiegelte Connection-IDs, Timeouts, Adoption |
| [internal/engine](internal/engine) | Engine-Lebenszyklus, Pools, Warm-Pool, Provider-Schnittstelle |
| [internal/engine/kubernetes](internal/engine/kubernetes) | Engines als Pods (Informer, Secrets, Phasen) |
| [internal/engine/process](internal/engine/process), [internal/engine/static](internal/engine/static) | lokale Prozesse bzw. extern betriebene Quack-Server |
| [internal/auth](internal/auth) | Authentifizierung `static` (Token-Hashes), `jwt` (Keys/JWKS), `none` |
| [internal/policy](internal/policy) | Optionen, Defaults, Overrides, Rechte |
| [internal/admin](internal/admin), [internal/ha](internal/ha) | Admin-API, Metriken, Leader-Election |
| [images/engine](images/engine) | Engine-Image (DuckDB 2.0 + quack/httpfs, tini, FIFO-Entrypoint, Autorisierungs-Makro für geteilte Engines) |
| [deploy/kubernetes](deploy/kubernetes) | Manifeste (RBAC, NetworkPolicy, Overlays `dev`/`ha`) |
| [examples](examples) | Beispielkonfigurationen und Client-SQL |
| [test/e2e](test/e2e) | End-to-End-Tests mit echtem DuckDB (Docker Compose, kind) |
| [docs](docs) | [Design](docs/design.md), [Protokoll](docs/protocol.md) |

## Tests

```powershell
go test ./... -count=1
```

Die Unit- und Integrationstests laufen gegen einen protokolltreuen Fake-Quack-Server und brauchen weder Docker
noch DuckDB. Die E2E-Tests in [test/e2e](test/e2e) prüfen das Zusammenspiel mit echten DuckDB-2.0-Clients und
-Engines. GitHub Actions führt alles bei jedem Pull Request aus, die HA-Szenarien nächtlich
([docs/releasing.md](docs/releasing.md)).
