# Übergabe: quackmux weiterbauen (Stand 2026-11-19)

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
den Research-Notes.

## Stand

**Fertig:**
* Alle Pakete kompilieren für windows/386, linux/amd64 und linux/arm64.
* `go vet ./...` und `gofmt -l .` sind sauber, `go test ./... -count=1` ist komplett grün.
* Getestet wurde nur unter Windows (32-bit Go). Der Race-Detector lief noch nie.

| Bereich | Pfad | Status |
|---|---|---|
| Wire-Codec (Header, Handshake, Fehler, Kontrollnachrichten) | `internal/wire` | fertig. Golden-Byte-Tests sind aus dem DuckDB-Serializer-Quellcode abgeleitet und nicht gegen echtes DuckDB geprüft. |
| Proxy (Handshake, Routing, Header-Rewrite, Puffern/Streaming, lokale Heartbeats/DISCONNECT, Audit) | `internal/proxy` | fertig, ca. 30 Integrationstests gegen den Fake-Server `internal/quacktest` |
| Sessions (versiegelte IDs ≤ 140 Zeichen, Timeouts, Lease, Keepalive, Adoption, Tombstones) | `internal/session` | fertig |
| Engine-Manager (Single-Flight-Start, Pools, Warm-Pool, Drain, Spec-Drift, Recovery-Grace, permanente Static-Engines) | `internal/engine` | fertig |
| Provider: Kubernetes, process (lokal), static, fake | `internal/engine/*` | fertig mit Unit-Tests (k8s über die fake clientset) |
| Auth: static (SHA-256-Token-Datei, Hot-Reload, SIGHUP), jwt (HS/RS/ES/EdDSA, JWKS), none | `internal/auth` | fertig |
| Policy (Optionen, Defaults, Overrides, Rechte), Config (YAML-Schema, Defaults, Validierung) | `internal/policy`, `internal/config` | fertig |
| Admin-API, Metriken, Leader-Election | `internal/admin`, `internal/metrics`, `internal/ha` | fertig |
| Binary | `cmd/quackmux` (`serve`, `validate`, `token`, `hash-token`, `version`) | fertig |
| Engine-Image (tini, FIFO-Entrypoint, quack/httpfs vorinstalliert), Gateway-Image | `images/engine`, `Dockerfile` | geschrieben, **nie gebaut** |
| Kustomize (base, overlays `dev` und `ha`), Beispielkonfigurationen | `deploy/kubernetes`, `examples` | geschrieben, Konfigurationen per `config.Parse` validiert, **nie deployt** |
| E2E-Tests (Docker Compose, kind) | `test/e2e` | geschrieben, **nie ausgeführt** |

In der lokalen Umgebung konnten weder Docker noch DuckDB-Binaries laufen. Alle Tests nutzen den
protokolltreuen Fake-Server.

## Harte Randbedingungen

Diese Regeln nicht brechen:

1. **Gateway-Connection-IDs ≤ 156 Bytes.** Sonst wirft der DuckDB-Client beim INSERT eine `InternalException` und
   invalidiert seine gesamte Datenbank (`quack_send_data.cpp:99`). `session.MaxIDLength` ist auf 140 gesetzt,
   ein Test sichert das ab.
2. **Fehler immer als HTTP 200 mit `ERROR_RESPONSE`.** Ein schon gelesener Request bekommt immer eine Antwort.
   Ein Verbindungsabbruch ohne Antwort kann libcurl einen POST wiederholen lassen, dann läuft eine Query doppelt.
3. **Routing nur über die `connection_id` im Header**, nie über TCP-Verbindungen. Clients schicken bis zu 256
   parallele FETCH-Long-Polls pro Session.
4. **Keine kurzen Timeouts auf PREPARE, FETCH oder SEND_DATA.** Sie blockieren serverseitig beliebig lange, der
   Client wartet bis zu 24 h.
5. **Puffern statt durchstreamen** (bis 100 MiB Request, 64 MiB Response, Budget 1 GiB). Die Engine bricht
   Verbindungen ab, die 5 s stocken.
6. **Client-Heartbeats und DISCONNECT beantwortet das Gateway lokal.** Die Engine-Lease (1 h) erhält es selbst
   per Keepalive.
7. **Fremde oder unbekannte Connection-IDs nie weiterleiten.** Nur versiegelte IDs (AES-GCM) werden geroutet.
8. Doku für Nutzer und Betreiber auf Deutsch, Code-Kommentare auf Englisch.

## Nächste Schritte (priorisiert)

1. **Mit echtem DuckDB 2.0 verifizieren.** In der Cloud sind Docker und Downloads hoffentlich erlaubt.
   * `make docker-engine docker-gateway`, dann `test/e2e/compose/run.sh`. Gefundene Probleme beheben.
   * Typische Kandidaten:
     * Entrypoint-Annahmen in `images/engine/entrypoint.sh` (`.read`/`-bail`, `getenv`, Parameternamen von
       `quack_serve`, SIGTERM-Pfad)
     * die genauen Fehlertexte, nach denen die E2E-Skripte greppen
     * die Byte-Layouts in `internal/wire`: mit echtem Traffic gegenprüfen, z. B. mit einem Mitschnitt zwischen
       DuckDB-Client und Gateway
   * Danach `test/e2e/kind/setup.sh` und `test.sh`.
2. **`go test -race ./...`** auf linux/amd64 laufen lassen. Unter windows/386 ging das nicht.
3. **Adversariales Gesamt-Review** mit getrennten Blickwinkeln:
   * Protokoll und Client-Kompatibilität
   * Nebenläufigkeit und Lebenszyklen (Engine-/Session-Manager, Lock-Reihenfolge, Zähler)
   * Fehlerfälle (Neustart, HA-Failover, Pod-Crash, halbe Launches)
   * Security und Mandantentrennung
   * Kubernetes-Provider und Manifeste

   Funde gegenprüfen, dann beheben.
4. **Bekannte Lücken und Ideen:**
   * **Zombie-Backend-Verbindungen nach einem Gateway-Neustart** bei geteilten Engines: Clients, die während der
     Downtime gestorben sind, werden nie adoptiert. Abhilfe wäre `quack_active_connections()` abzufragen. Das
     verlangt das Dekodieren von DataChunks (VARCHAR) aus `PREPARE_RESPONSE`. Bis dahin hilft
     `engine.maxLifetime`.
   * Eine Warm-Engine, die geclaimt und nach einem Neustart wiedergefunden wurde, hat einen leeren `User`.
     Per-User-Engine-Limits zählen sie dann nicht.
   * Optional:
     * Query-/Request-Timeout (CANCEL nach X)
     * Helm-Chart
     * Härtung geteilter Engines (`quack_authorization_function`-Makro gegen `quack_stop` und Co.)
     * TLS-Doku für Clients mit `ssl_fingerprint` (die haben nur 30 s `http_timeout`, deshalb Warm-Pool nutzen)
5. Danach: README und Doku nachziehen, Release-Workflow (GitHub Actions: build, vet, test, race, Images)
   ergänzen.

## Arbeiten im Repo

```bash
go build ./... && go vet ./... && go test ./... -count=1
go run ./cmd/quackmux validate -config examples/config.local.yaml
make help   # oder Makefile ansehen: build, test, docker-engine, docker-gateway, e2e-compose, kind-up/test/down
```

* Die Tests brauchen weder Docker noch DuckDB. `internal/proxy/harness_test.go` baut ein komplettes Gateway mit
  Fake-Engines und Fake-Uhr.
* Während der Entwicklung lag der Quellcode von duckdb-quack unter
  `https://github.com/duckdb/duckdb-quack` (Commit `02c9d4a`). Der Serializer von DuckDB liegt in
  `duckdb/duckdb@181615215e5e/src/common/serializer/`.

---

**Prompt für die Cloud-Session** (zum Einfügen):

> Lies `docs/HANDOVER.md` und die dort verlinkten Dokumente. Arbeite dann die „Nächsten Schritte“ in der
> angegebenen Reihenfolge ab und beginne mit der Verifikation gegen echtes DuckDB 2.0 (Docker-Images bauen,
> `test/e2e/compose/run.sh`). Halte dich an die „Harten Randbedingungen“. Antworte mir auf Deutsch.
