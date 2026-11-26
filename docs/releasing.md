# CI und Releases

## Automatische Prüfungen

| Workflow | Auslöser | Inhalt |
|---|---|---|
| [ci.yml](../.github/workflows/ci.yml) | jeder Pull Request, jeder Push auf `main`, von Hand | `gofmt`, `go mod tidy`, `go vet`, Tests, Tests mit Race-Detector, bekannte Schwachstellen (`govulncheck`), Builds für linux/amd64, linux/arm64, darwin/arm64, windows/amd64 und windows/386, `quackmux validate` für alle mitgelieferten Konfigurationen, die gerenderten Kustomize-Overlays gegen die Kubernetes-Schemas (kubeconform), das Helm-Chart mit Helm 3 und 4 (`helm lint`, kubeconform, Vergleich mit den Kustomize-Manifesten in `test/helm`), Image-Referenzen, shellcheck, actionlint, E2E mit Docker Compose und mit kind (dort auch das Helm-Chart mit `test/e2e/kind/helm.sh`) |
| [nightly.yml](../.github/workflows/nightly.yml) | täglich 03:17 UTC auf `main`, von Hand für jeden Branch | HA-Szenarien in kind (`test/e2e/kind/ha.sh`), die Tests fünfmal mit Race-Detector und `govulncheck` für neu gemeldete Schwachstellen |
| [release.yml](../.github/workflows/release.yml) | ein veröffentlichtes GitHub-Release `vX.Y.Z`; von Hand als Probelauf | Release (siehe unten) |

Alle Actions sind auf Commit-SHAs gepinnt, die Werkzeuge (kind, kubeconform, actionlint, helm, govulncheck) auf
Versionen. Der Engine-Build prüft den DuckDB-Download gegen die Prüfsummen im Makefile (`DUCKDB_SHA256_AMD64`,
`DUCKDB_SHA256_ARM64`); die E2E-Skripte und Workflows lesen sie von dort.

`govulncheck` meldet nur Schwachstellen in Code, den quackmux aufruft, und in der Go-Version aus `go.mod`. Schlägt es
an, das betroffene Modul (`go get <modul>@<version>`, `go mod tidy`) oder die Go-Version in `go.mod` anheben. Die
Images betrifft das nicht: Ihre Basis (`golang`, `distroless`, `debian:bookworm-slim`) holt erst der nächste Build.
Nach einer Sicherheitslücke in der Basis also ein Patch-Release bauen.

[Dependabot](../.github/dependabot.yml) schlägt wöchentlich Updates für die Go-Module, die Actions und die Basis des
Gateway-Images vor, frühestens eine Woche nach dem Erscheinen einer Version. Sofortige Sicherheitsupdates schaltet
man in den Einstellungen des Repositorys ein („Dependabot security updates“).

## Was ein Release veröffentlicht

Ein veröffentlichtes GitHub-Release mit einem Tag `vX.Y.Z` startet `release.yml` (siehe „Ablauf“):

1. Die Checks laufen noch einmal, und `scripts/check-image-refs.sh X.Y.Z` prüft, dass das Repository genau dieses
   Release referenziert, auch `version` und `appVersion` des Helm-Charts.
2. Am GitHub-Release: die Binaries für linux, darwin und windows (amd64 und arm64) als `tar.gz` bzw. `zip`
   (jeweils mit `LICENSE`, `NOTICE` und `THIRD_PARTY_LICENSES`, den Lizenzen der Go-Module aus
   [scripts/third-party-licenses.sh](../scripts/third-party-licenses.sh); ein Modul ohne Lizenzdatei bricht das
   Release ab),
   dem Helm-Chart als `quackmux-X.Y.Z.tgz` ([deploy/helm/quackmux](../deploy/helm/quackmux/README.md)) und
   `SHA256SUMS`.
3. Images für amd64 und arm64 auf der GitHub Container Registry:

| Image | Tags |
|---|---|
| `ghcr.io/lldeters/quackmux` | `X.Y.Z`, `X.Y`, `latest` |
| `ghcr.io/lldeters/duckdb-engine` | `<duckdb>-X.Y.Z`, `<duckdb>` (zum Beispiel `2.0.0-0.1.0`, `2.0.0`) |

Vorabversionen (`vX.Y.Z-rc.1`) bekommen nur die vollen Tags und werden als Pre-Release markiert.

Die Manifeste, die Beispielkonfiguration und das Default-Engine-Image im Code (`config.DefaultEngineImage`)
referenzieren die **unveränderlichen** Tags eines Releases: `ghcr.io/lldeters/quackmux:X.Y.Z` und
`ghcr.io/lldeters/duckdb-engine:<duckdb>-X.Y.Z`. Gateway und Engine-Image gehören zusammen. Der Entrypoint des
Engine-Images setzt den Engine-Container-Vertrag um (etwa `QUACKMUX_STOP_TIMEOUT`); ein bewegliches Tag wie
`2.0.0` könnte auf einem Node mit `IfNotPresent` noch eine ältere Fassung liefern.

## Ablauf

```sh
scripts/set-version.sh 0.2.0     # alle Image-Referenzen und die Chart-Version auf das neue Release
git commit -am "release: 0.2.0"  # über einen Pull Request nach main, CI muss grün sein
```

Dazu die Release-Notes als `docs/releases/0.2.0.md` anlegen (Vorbild: [docs/releases](releases)). Nach dem
Merge auf `main` das Release veröffentlichen:

* **In der Weboberfläche:** „Releases“ → „Draft a new release“ → unter „Choose a tag“ `v0.2.0` eintippen und
  „Create new tag: v0.2.0 on publish“ wählen, Target `main`. Titel und Beschreibung leer lassen: Der Workflow
  setzt den Titel „quackmux v0.2.0“ und die Notes aus `docs/releases/0.2.0.md`. Eine selbst geschriebene
  Beschreibung bleibt. Bei einer Vorabversion (`v0.2.0-rc.1`) „Set as a pre-release“ ankreuzen. Dann „Publish
  release“.
* **Mit der Kommandozeile:** `gh release create v0.2.0 --target main --notes ""`.

Erst das Veröffentlichen startet den Workflow. Ein Tag allein, ein Entwurf (Draft) oder ein `git push` eines Tags
veröffentlicht nichts. Der Workflow prüft und baut alles und hängt dann Binaries, Chart und `SHA256SUMS` an das
Release; die Images gehen nach GHCR. Scheitert er, bleibt das Release ohne Dateien: den Fehler beheben und unter
„Actions“ den Lauf mit „Re-run failed jobs“ wiederholen (oder das Release samt Tag löschen und neu anlegen).

**Probelauf vorab:** „Actions“ → „Release“ → „Run workflow“ auf einem Branch. Der Lauf nimmt die Version aus
`deploy/helm/quackmux/Chart.yaml`, macht dieselben Prüfungen und Builds (Binaries, Chart, beide Images für amd64
und arm64), legt aber kein Release an und schiebt keine Images.

Danach den Workflow unter „Actions“ verfolgen. Beim ersten Release legt GitHub die Pakete an. Ist eines
privat, stellt man es unter „Packages“ → Paket → „Package settings“ → „Change visibility“ auf öffentlich, sonst
brauchen Cluster ein Pull-Secret.

## DuckDB-Version wechseln

1. `DUCKDB_VERSION` im Makefile setzen und die Prüfsummen der beiden CLI-Archive eintragen:

   ```sh
   for a in amd64 arm64; do
     curl -fsSLO "https://github.com/duckdb/duckdb/releases/download/v<version>/duckdb-cli-linux-$a.tar.gz"
     sha256sum "duckdb-cli-linux-$a.tar.gz"
   done
   ```

2. `scripts/set-version.sh <nächste quackmux-Version>` setzt auch das Engine-Tag (`<duckdb>-<version>`).
3. Die Research-Notes und die E2E-Tests gegen die neue Version prüfen (docs/research-notes.md).
4. docs/tls.md nennt die geprüfte DuckDB- und quack-Version. Szenario l des Compose-Tests prüft die Clients mit
   `ssl_fingerprint` und mit `ca_cert_file` (Header, Pin-Formate, `localhost`, `http_timeout`, Fehlermeldungen);
   die Timeouts ohne httpfs und über 30 s deckt es nicht ab: von Hand nachprüfen, dann die Version dort anpassen.
