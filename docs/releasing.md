# CI und Releases

## Automatische Prüfungen

| Workflow | Auslöser | Inhalt |
|---|---|---|
| [ci.yml](../.github/workflows/ci.yml) | jeder Pull Request, jeder Push auf `main`, von Hand | `gofmt`, `go vet`, Tests, Tests mit Race-Detector, Builds für linux/amd64, linux/arm64, darwin/arm64, windows/amd64 und windows/386, `quackmux validate` für alle mitgelieferten Konfigurationen, die gerenderten Kustomize-Overlays gegen die Kubernetes-Schemas (kubeconform), das Helm-Chart mit Helm 3 und 4 (`helm lint`, kubeconform, Vergleich mit den Kustomize-Manifesten in `test/helm`), Image-Referenzen, shellcheck, actionlint, E2E mit Docker Compose und mit kind (dort auch das Helm-Chart mit `test/e2e/kind/helm.sh`) |
| [nightly.yml](../.github/workflows/nightly.yml) | täglich 03:17 UTC auf `main`, von Hand für jeden Branch | HA-Szenarien in kind (`test/e2e/kind/ha.sh`) und die Tests fünfmal mit Race-Detector |
| [release.yml](../.github/workflows/release.yml) | Tag `vX.Y.Z` | Release (siehe unten) |

Alle Actions sind auf Commit-SHAs gepinnt, die Werkzeuge (kind, kubeconform, actionlint, helm) auf Versionen. Der
Engine-Build prüft den DuckDB-Download gegen die Prüfsummen im Makefile (`DUCKDB_SHA256_AMD64`,
`DUCKDB_SHA256_ARM64`); die E2E-Skripte und Workflows lesen sie von dort.

## Was ein Release veröffentlicht

Ein Tag `vX.Y.Z` startet `release.yml`:

1. Die Checks laufen noch einmal, und `scripts/check-image-refs.sh X.Y.Z` prüft, dass das Repository genau dieses
   Release referenziert, auch `version` und `appVersion` des Helm-Charts.
2. Ein GitHub-Release mit den Binaries für linux, darwin und windows (amd64 und arm64) als `tar.gz` bzw. `zip`
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

Nach dem Merge auf `main`:

```sh
git tag -a v0.2.0 -m "quackmux 0.2.0"
git push origin v0.2.0
```

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
