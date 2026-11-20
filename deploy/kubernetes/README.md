# quackmux auf Kubernetes

Kustomize-Manifeste für das quackmux-Gateway. Das Gateway startet die Engines (DuckDB 2.0 mit
`quack_serve`) selbst als Pods in seinem Namespace und räumt sie nach Timeouts wieder ab.

```
deploy/kubernetes/
├── base/                 Gateway (1 Replika), RBAC, Service, NetworkPolicies
│   ├── config.yaml       Gateway-Konfiguration (wird zur ConfigMap quackmux-config)
│   └── secrets/          PLATZHALTER: Session-Key, Admin-Token, users.yaml
└── overlays/
    ├── ha/               2 Replikas mit Leader-Election, PodDisruptionBudget
    └── dev/              kind: ohne Authentifizierung, kleine Ressourcen
```

Alle Konfigurationsoptionen mit ihren Defaults stehen in
[examples/config.kubernetes.yaml](../../examples/config.kubernetes.yaml), Client-Beispiele in
[examples/client.sql](../../examples/client.sql).

## Voraussetzungen

* Kubernetes 1.30 oder neuer, `kubectl` 1.27 oder neuer (enthält Kustomize v5).
* Ein CNI, das NetworkPolicies durchsetzt (Calico, Cilium …). Sonst wirken die Policies nicht, und Pods im
  Cluster könnten Engines unter Umgehung des Gateways erreichen.
* Images:
  * Gateway `quackmux/quackmux:0.1.0`. Der Entrypoint des Images muss das `quackmux`-Binary sein; das
    Deployment übergibt nur die Argumente `serve -config /etc/quackmux/config.yaml`.
  * Engine `quackmux/duckdb-engine:2.0.0` aus [images/engine](../../images/engine), je Profil in
    `config.yaml` eingetragen (`profiles.<name>.image`).
  * Eigene Registry: das Gateway-Image per `kustomize edit set image quackmux/quackmux=registry.example.com/quackmux:1.2.3`
    (bzw. `images:` im Overlay), das Engine-Image in `config.yaml`. Private Registries brauchen
    `imagePullSecrets`: für das Gateway per Patch des Deployments, für Engines in `profiles.<name>.imagePullSecrets`.

## Installation

1. Echte Secrets erzeugen (nächster Abschnitt). Die Dateien unter `base/secrets/` sind öffentlich bekannte
   Platzhalter.
2. Anwenden und warten:

   ```sh
   kubectl apply -k deploy/kubernetes/base        # bzw. Ihr eigenes Overlay
   kubectl -n quackmux rollout status deployment/quackmux
   kubectl -n quackmux get pods
   ```

Das Gateway meldet sich bereit (`1/1 READY`), sobald seine Engine-Registry synchronisiert ist. Engine-Pods
entstehen erst beim ersten `ATTACH`, etwa `quack-user-alice-x7k2p` oder `quack-connection-conn-x7k2p`. Ohne
Sessions beendet das Gateway sie nach `engine.idleTimeout` (USER/GROUP/SERVER 30 min, CONNECTION sofort).

| Objekt | Zweck |
|---|---|
| Namespace `quackmux` | Gateway und Engines. Pod Security `restricted` wird durchgesetzt. |
| ServiceAccount, Role, RoleBinding `quackmux-gateway` | Engine-Pods und ihre Token-Secrets verwalten, Events schreiben, Lease (HA) |
| ConfigMap `quackmux-config-<hash>` | `config.yaml`, gemountet als `/etc/quackmux/config.yaml` |
| Secrets `quackmux-session-key-<hash>`, `quackmux-admin-token-<hash>`, `quackmux-users` | `/etc/quackmux/session/key`, `/etc/quackmux/admin/token`, `/etc/quackmux/users/users.yaml` |
| Deployment `quackmux` | Gateway, 1 Replika, Strategie `Recreate` |
| Service `quackmux` | ClusterIP: 9494 (Quack), 9495 (Admin-API, Probes, Metriken) |
| NetworkPolicies | Engines nur vom Gateway auf 9494; Gateway: 9494 von überall, 9495 nur aus dem Namespace |

Warum `Recreate` und nur eine Replika? Ohne Leader-Election darf genau ein Gateway die Engines verwalten. Ein
Update unterbricht den Betrieb daher kurz. Die Sessions überleben das (versiegelte Connection-IDs, gemeinsamer
Session-Key); nur gerade laufende Requests brechen ab. Zwei Replikas: siehe
[Hochverfügbarkeit](#hochverfügbarkeit-overlaysha).

Ressourcen des Gateways: Requests 250m CPU / 512 MiB, Limits 2 CPU / 2 GiB, `GOMEMLIMIT=1800MiB`. Der größte
Verbraucher ist der Nachrichtenpuffer (`server.upstream.bufferBudgetBytes`, 1 GiB). Wer das Speicherlimit
senkt, senkt das Budget und `GOMEMLIMIT` mit (Beispiel: `overlays/dev`).

## Echte Secrets erzeugen

Die Werkzeuge stecken im Binary (`go build -o bin/quackmux ./cmd/quackmux`):

```sh
quackmux token                            # neues Token und sein sha256
# token:  …43 Zeichen…   -> bekommt der Benutzer
# sha256: …64 Hex…       -> kommt in users.yaml
quackmux hash-token 'vorhandenes-token'   # sha256 eines bestehenden Tokens
```

Legen Sie ein eigenes Overlay außerhalb dieses Repositorys an und checken Sie echte Secrets nirgends ein
(oder erzeugen Sie die Dateien in der Pipeline aus einem Tresor):

```
meine-umgebung/
├── kustomization.yaml
└── secrets/
    ├── session-key
    ├── admin-token
    └── users.yaml
```

```sh
mkdir -p meine-umgebung/secrets
quackmux token | sed -n 's/^token: *//p' > meine-umgebung/secrets/session-key
quackmux token | sed -n 's/^token: *//p' > meine-umgebung/secrets/admin-token
```

PowerShell:

```powershell
(bin\quackmux.exe token | Select-String '^token:').Line.Split()[-1] | Out-File -Encoding ascii -NoNewline meine-umgebung\secrets\session-key
(bin\quackmux.exe token | Select-String '^token:').Line.Split()[-1] | Out-File -Encoding ascii -NoNewline meine-umgebung\secrets\admin-token
```

`meine-umgebung/secrets/users.yaml` (Format und Regeln: [examples/users.yaml](../../examples/users.yaml)):

```yaml
users:
  - name: alice
    groups: [analytics]            # die erste Gruppe ist der Default für share_level=group
    tokens:
      - sha256: "<sha256 aus quackmux token>"
        description: "Laptop"
        expires: "2027-06-30T00:00:00Z"   # optional
```

`meine-umgebung/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../quack-multiplexer/deploy/kubernetes/base     # oder .../overlays/ha
secretGenerator:
  - name: quackmux-session-key
    behavior: replace
    files: [key=secrets/session-key]
  - name: quackmux-admin-token
    behavior: replace
    files: [token=secrets/admin-token]
  - name: quackmux-users
    behavior: replace
    files: [users.yaml=secrets/users.yaml]
```

```sh
kubectl apply -k meine-umgebung
```

* **Session-Key:** mindestens 16 Bytes, besser 32 Zufallsbytes (`quackmux token` liefert 32). Alle Replikas
  brauchen denselben. Ein neuer Key beendet alle Sessions: Die Clients bekommen „Invalid connection id“ und
  müssen neu attachen.
* **Admin-Token:** schützt `/api/v1/…`. Eine Änderung erzeugt einen neuen Secret-Namen und rollt das Gateway
  neu aus.
* **users.yaml:** Dieses Secret hat keinen Hash-Suffix. `kubectl apply` aktualisiert es an Ort und Stelle;
  Kubelet und Gateway (`auth.static.reloadInterval`) übernehmen die Änderung nach etwa einer Minute, ohne
  Neustart. Eine ungültige Datei wird abgelehnt und die bisherigen Benutzer bleiben aktiv; der Grund steht im
  Gateway-Log.
* Leerzeichen und Zeilenumbrüche am Dateiende ignoriert das Gateway.
* Die Base lässt sich auch remote einbinden, z. B.
  `https://github.com/<org>/quack-multiplexer//deploy/kubernetes/base?ref=<tag>`.

## Verbinden

### Port-Forward (zum Testen)

```sh
kubectl -n quackmux port-forward svc/quackmux 9494:9494 9495:9495
```

```sql
CREATE SECRET (TYPE quack, TOKEN 'change-me-example-token-0000', SCOPE 'quack:localhost:9494');
ATTACH 'quack:localhost:9494' AS lake;
FROM lake.query('SELECT 42');
DETACH lake;
```

`localhost` spricht der Client per HTTP an. Das Token ist das öffentliche Beispiel-Token aus
`base/secrets/users.yaml`; mit Ihrem eigenen Overlay gilt Ihr eigenes.

### Im Cluster

Andere Pods erreichen das Gateway unter `quackmux.quackmux.svc.cluster.local:9494`. Für jeden Host außer
`localhost` erwartet der DuckDB-Client HTTPS; ohne `server.tls` spricht das Gateway Plain HTTP, also:

```sql
CREATE SECRET (TYPE quack, TOKEN '…', SCOPE 'quack:quackmux.quackmux.svc.cluster.local:9494');
ATTACH 'quack:quackmux.quackmux.svc.cluster.local:9494' AS lake (DISABLE_SSL true);
```

### Von außen: L4-Load-Balancer

`base/service.yaml` enthält auskommentiert den Service `quackmux-external` vom Typ `LoadBalancer` (nur Port
9494, `externalTrafficPolicy: Local`). Übernehmen Sie ihn in Ihr Overlay. Mit Plain HTTP gehen die Tokens im
Klartext über das Netz; außerhalb vertrauenswürdiger Netze gehört TLS davor:

* **TLS am Load Balancer:** Ein L4-Load-Balancer (TCP), der TLS terminiert und auf Port 9494 der Pods
  weiterleitet. Die Clients verbinden sich dann ohne `DISABLE_SSL`.
* **TLS im Gateway (`server.tls`):** Ein Zertifikat als Secret einbinden, z. B. von cert-manager
  (`tls.crt`, `tls.key`). Patch im eigenen Overlay:

  ```yaml
  apiVersion: apps/v1
  kind: Deployment
  metadata:
    name: quackmux
  spec:
    template:
      spec:
        containers:
          - name: gateway
            volumeMounts:
              - name: tls
                mountPath: /etc/quackmux/tls
                readOnly: true
        volumes:
          - name: tls
            secret:
              secretName: quackmux-tls
              defaultMode: 0440
  ```

  und in der Konfiguration:

  ```yaml
  server:
    tls:
      certFile: /etc/quackmux/tls/tls.crt
      keyFile: /etc/quackmux/tls/tls.key
  ```

  Das Zertifikat muss zum Hostnamen passen und für den Client vertrauenswürdig sein. Das Gateway lädt es beim
  Start; nach einer Erneuerung `kubectl -n quackmux rollout restart deployment/quackmux` (im HA-Modus mit
  kontrolliertem Failover, siehe unten).
* **Idle-Timeout des Load Balancers:** Während die Engine rechnet, fließen in einem FETCH-Request keine Bytes.
  Das Idle-Timeout muss deutlich über der längsten erwarteten Rechenzeit liegen (anbieterspezifische
  Annotation, Beispiel in `service.yaml`).

> **Kein L7-Ingress vor das Gateway.** HTTP-Ingress-Controller und L7-Load-Balancer haben typischerweise
> Request-Timeouts von 30–60 s und Body-Limits um 1 MiB (ingress-nginx: `proxy-read-timeout` 60 s,
> `proxy-body-size` 1m). Ein FETCH blockiert aber, bis die Engine den nächsten Batch fertig hat (bei langen
> Queries minutenlang), und `SEND_DATA`-Requests (Daten vom Client, z. B. Inserts) sind bis zu 100 MiB groß.
> Der Client meldet dann nur „Failed to send message“. Wenn es nicht anders geht: Timeouts auf Stunden,
> Body-Limit auf mindestens 100 MiB, Request-Buffering aus (ingress-nginx:
> `nginx.ingress.kubernetes.io/proxy-read-timeout`, `…/proxy-send-timeout`, `…/proxy-body-size`,
> `…/proxy-request-buffering: "off"`).

## Share-Levels und Optionen

| Share-Level | Engine | Hinweis |
|---|---|---|
| `connection` | eigener Pod pro `ATTACH`, endet mit dem `DETACH` | stärkste Isolation; ein Pod-Start pro Verbindung (vorgestartet: `profiles.<name>.warmPool.size`) |
| `user` (Default) | ein Pod pro Benutzer (je Subdomain und Profil) | |
| `group` | ein Pod pro Gruppe | Option `group`, sonst die erste Gruppe des Benutzers |
| `server` | ein Pod für alle (je Subdomain und Profil) | |

Optionen im Token funktionieren mit jedem Client-Transport:

```sql
CREATE SECRET (TYPE quack, TOKEN 'mein-token?share_level=connection&profile=large',
               SCOPE 'quack:quackmux.example.com:9494');
```

Alternativ als HTTP-Header über `EXTRA_HTTP_HEADERS` (nicht zusammen mit `ssl_fingerprint`-Pinning; dieser
Client-Pfad sendet keine Zusatz-Header):

```sql
CREATE SECRET (TYPE quack, TOKEN 'mein-token', SCOPE 'quack:quackmux.example.com:9494',
               EXTRA_HTTP_HEADERS MAP {'X-Quackmux-Share-Level': 'server', 'X-Quackmux-Subdomain': 'bi'});
```

| Option | Header | Werte |
|---|---|---|
| `share_level` | `X-Quackmux-Share-Level` | `connection`, `user`, `group`, `server` |
| `subdomain` | `X-Quackmux-Subdomain` | `[a-z0-9-]`, max. 32 Zeichen (Default `default`) |
| `profile` | `X-Quackmux-Profile` | ein Profil aus `config.yaml` (Base: `default`, `large`) |
| `group` | `X-Quackmux-Group` | eine Gruppe des Benutzers |

Priorität: Token-Optionen vor Headern, Header vor `overrides[].defaults`, diese vor `defaults`. Was ein
Benutzer wählen darf, regeln `profiles.<name>.allowedShareLevels`, `allowedUsers`, `allowedGroups` und
`overrides` (in der Base: `large` nur mit `user` oder `connection`). Verstöße meldet der Client als
`quackmux: not permitted: …`.

## Admin-API

Port 9495, Bearer-Token aus dem Secret `quackmux-admin-token`; `/healthz`, `/readyz` und `/metrics` brauchen
keins. Die NetworkPolicy lässt Port 9495 nur aus dem Namespace zu, von außen also per Port-Forward:

```sh
kubectl -n quackmux port-forward svc/quackmux 9495:9495
TOKEN=$(cat meine-umgebung/secrets/admin-token)
# oder aus dem Secret, das das Deployment gerade verwendet:
# SECRET=$(kubectl -n quackmux get deployment quackmux \
#            -o jsonpath='{.spec.template.spec.volumes[?(@.name=="admin-token")].secret.secretName}')
# TOKEN=$(kubectl -n quackmux get secret "$SECRET" -o jsonpath='{.data.token}' | base64 -d)

curl -s -H "Authorization: Bearer $TOKEN" http://localhost:9495/api/v1/info
curl -s -H "Authorization: Bearer $TOKEN" "http://localhost:9495/api/v1/sessions?user=alice"
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:9495/api/v1/engines
curl -s -X POST   -H "Authorization: Bearer $TOKEN" http://localhost:9495/api/v1/sessions/<session-id>/cancel
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" "http://localhost:9495/api/v1/sessions/<session-id>?force=true"
curl -s -X POST   -H "Authorization: Bearer $TOKEN" http://localhost:9495/api/v1/engines/quack-user-alice-x7k2p/drain
```

PowerShell:

```powershell
$token = (Get-Content -Raw meine-umgebung\secrets\admin-token).Trim()
curl.exe -s -H "Authorization: Bearer $token" http://localhost:9495/api/v1/engines
```

| Methode und Pfad | Wirkung |
|---|---|
| `GET /api/v1/info` | Version, Anzahl der Sessions und Engines nach Zustand |
| `GET /api/v1/sessions[?user=…][&engine=…]` | offene Sessions |
| `GET /api/v1/sessions/{id}` | eine Session samt Engine |
| `DELETE /api/v1/sessions/{id}[?force=true]` | Session schließen (409, solange Requests laufen, außer mit `force=true`) |
| `POST /api/v1/sessions/{id}/cancel` | laufende Query der Session abbrechen |
| `GET /api/v1/engines` | alle Engines mit Zustand, Sessions und ob ihre Spec aktuell ist |
| `GET /api/v1/engines/{id}` | eine Engine samt Sessions |
| `POST /api/v1/engines/{id}/drain` | keine neuen Sessions; die Engine endet mit ihrer letzten Session (spätestens nach `engine.drainTimeout` werden die Sessions geschlossen) |
| `DELETE /api/v1/engines/{id}` | Engine sofort beenden und ihre Sessions schließen |

## Metriken

`GET /metrics` auf Port 9495 liefert das Prometheus-Format ohne Token. Das Pod-Template trägt die Annotationen
`prometheus.io/scrape`, `prometheus.io/port` und `prometheus.io/path`. Mit dem Prometheus Operator:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: quackmux
  namespace: quackmux
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: quackmux
      app.kubernetes.io/component: gateway
  endpoints:
    - port: admin
      path: /metrics
```

Läuft Prometheus in einem anderen Namespace, braucht die Policy `quackmux-gateway` in
`base/networkpolicy.yaml` für Port 9495 zusätzlich einen `namespaceSelector` (Beispiel im Kommentar der Datei).

| Metrik | Bedeutung |
|---|---|
| `quackmux_sessions{share_level,profile}` | offene Sessions |
| `quackmux_sessions_opened_total{share_level,profile,adopted}` | geöffnete Sessions (`adopted`: nach einem Neustart übernommen) |
| `quackmux_sessions_closed_total{reason}` | geschlossene Sessions, z. B. `client disconnect`, `idle timeout`, `heartbeat lease expired`, `engine` |
| `quackmux_engines{state,share_level,profile}` | Engines je Zustand (`STARTING`, `READY`, `DRAINING`, `FAILED`, `TERMINATING`, Warm-Pool: `WARM_…`) |
| `quackmux_engine_starts_total{profile,result}`, `quackmux_engine_start_seconds{profile}` | Engine-Starts und ihre Dauer |
| `quackmux_engines_terminated_total{reason}` | beendete Engines (`idle`, `drained`, `failed`, `admin` …) |
| `quackmux_requests_total{type,outcome}`, `quackmux_request_seconds{type}` | Quack-Requests nach Nachrichtentyp |
| `quackmux_bytes_total{direction}` | durchgereichte Bytes (`in`: Client→Engine, `out`: Engine→Client) |
| `quackmux_connect_failures_total{reason}`, `quackmux_auth_failures_total{method}` | gescheiterte Handshakes, fehlgeschlagene Authentifizierungen |
| `quackmux_requests_in_flight`, `quackmux_is_leader` | laufende Requests; 1 auf der Replika, die bedient |

Das Audit-Log (`audit.enabled`, in der Base an) schreibt jede Query als JSON-Zeile mit Benutzer, Session,
Engine und gekürztem SQL ins Gateway-Log:

```sh
kubectl -n quackmux logs deployment/quackmux | grep '"component":"audit"'
```

## Hochverfügbarkeit (`overlays/ha`)

```sh
kubectl apply -k deploy/kubernetes/overlays/ha     # bzw. Ihr Overlay auf dieser Basis
```

* Zwei Replikas wählen über die Lease `quackmux-default` einen Leader. **Nur der Leader ist ready** und bekommt
  über den Service Traffic. Der Follower zeigt `0/1 READY` und Readiness-Fehler (`not the leader`); das ist
  gewollt. Deshalb meldet `kubectl rollout status` (wie auch der Health-Check von Argo CD) für dieses Overlay
  nie Erfolg: Es wartet auf „1 of 2 updated replicas are available“. Den Zustand zeigt
  `kubectl -n quackmux get deployment quackmux`: fertig bei `UP-TO-DATE 2`, `AVAILABLE 1`; die Bedingung
  `Available` ist dann erfüllt (`kubectl -n quackmux wait --for=condition=Available deployment/quackmux`).
* Leader anzeigen: `kubectl -n quackmux get lease quackmux-default -o jsonpath='{.spec.holderIdentity}'`
  (oder die Metrik `quackmux_is_leader`).
* Fällt der Leader aus, übernimmt der Follower spätestens nach `ha.leaseDuration` (15 s); fährt der Leader
  geordnet herunter, gibt er die Lease frei und die Übernahme dauert nur Sekunden. Kann ein Leader die Lease
  nicht verlängern, beendet sich sein Prozess und startet als Follower neu. Der neue Leader findet die Engines
  wieder und übernimmt die Sessions mit deren nächstem Heartbeat. Laufende Requests scheitern mit einem
  Transportfehler, die Sessions überleben.
* Eine bevorzugte `podAntiAffinity` verteilt die Replikas auf verschiedene Nodes.
* `overlays/ha/config.yaml` ersetzt `base/config.yaml` vollständig. Halten Sie beide Dateien bis auf den
  Abschnitt `ha` gleich.

**Rollout (neues Image, neue Konfiguration):** Weil nur der Leader ready ist, zählt Kubernetes nur einen der
beiden Pods als verfügbar. Mit `maxUnavailable: 1` und `maxSurge: 0` ersetzt der Rollout zuerst den Follower
und wartet dann: Der neue Pod wird nicht ready, solange der alte Leader die Lease hält. Sobald der neue Pod
läuft (`Running`, `0/1 READY`), schließen Sie den Rollout mit einem kontrollierten Failover ab:

```sh
kubectl apply -k deploy/kubernetes/overlays/ha
kubectl -n quackmux get pods -l app.kubernetes.io/component=gateway   # bis der neue Pod Running ist (0/1)
LEADER=$(kubectl -n quackmux get lease quackmux-default -o jsonpath='{.spec.holderIdentity}')
kubectl -n quackmux delete pod "$LEADER"      # gibt die Lease frei, der neue Pod übernimmt
kubectl -n quackmux get deployment quackmux   # fertig bei UP-TO-DATE 2, AVAILABLE 1
```

Wer vollautomatische Rollouts mit kurzer Unterbrechung vorzieht, setzt im eigenen Overlay
`maxUnavailable: 100%`; dann ersetzt Kubernetes beide Pods auf einmal, wie bei `Recreate`.

**Node-Wartung:** Das PodDisruptionBudget (`minAvailable: 1`) verhindert, dass `kubectl drain` den Leader
verdrängt; der Follower darf jederzeit gehen. Vor dem Drain des Leader-Nodes den Leader wie oben löschen.
Automatische Node-Upgrades mancher Anbieter warten auf die PDB oder verdrängen nach einer Frist trotzdem.

**Umstieg von der Base:** Zuerst das einzelne Gateway stoppen, damit nie ein Gateway ohne Leader-Election
neben einem Leader läuft:

```sh
kubectl -n quackmux scale deployment/quackmux --replicas=0
kubectl apply -k deploy/kubernetes/overlays/ha
```

## Entwicklung mit kind (`overlays/dev`)

`auth.mode: none`, Admin-API ohne Token, kleine Ressourcen (Gateway bis 512 MiB, Engines 100m/256 MiB
angefordert, bis 1 CPU/1 GiB), höchstens 8 Engines, Debug-Logs im Textformat. **Nur auf dem eigenen Rechner
verwenden:** Jeder Client darf sich jeden Benutzernamen aussuchen.

```sh
kind create cluster --name quackmux
docker build -t quackmux/quackmux:dev .                       # Gateway (Dockerfile im Repository-Root)
docker build -t quackmux/duckdb-engine:2.0.0 images/engine    # Engine
kind load docker-image --name quackmux quackmux/quackmux:dev quackmux/duckdb-engine:2.0.0
kubectl apply -k deploy/kubernetes/overlays/dev
kubectl -n quackmux rollout status deployment/quackmux
kubectl -n quackmux port-forward svc/quackmux 9494:9494 9495:9495
```

Jedes Token ist gültig, den Benutzer wählt die Option `user`:

```sql
CREATE SECRET (TYPE quack, TOKEN 'dev?user=alice', SCOPE 'quack:localhost:9494');
ATTACH 'quack:localhost:9494' AS lake;
```

```sh
curl -s http://localhost:9495/api/v1/engines        # ohne Token
```

Mit `imagePullPolicy: IfNotPresent` nutzt kind die geladenen Images und zieht nichts nach. Nach einem Neubau
`kind load` wiederholen und
`kubectl -n quackmux rollout restart deployment/quackmux`. Laufende Engines behalten ihr altes Image, bis sie
enden; `kubectl -n quackmux delete pod -l app.kubernetes.io/component=engine` beendet sie sofort (ihre Clients
müssen neu attachen). Die End-to-End-Tests in [test/e2e/kind](../../test/e2e/kind) nutzen dieses Overlay.

## Konfiguration ändern

* `base/config.yaml` (und `overlays/ha/config.yaml`) bearbeiten oder im eigenen Overlay per
  `configMapGenerator` mit `behavior: replace` eine eigene Datei einsetzen.
* Vorher prüfen: `quackmux validate -config meine-umgebung/config.yaml`. Unbekannte Felder sind Fehler.
* `kubectl apply -k …` erzeugt eine neue ConfigMap (neuer Hash) und rollt das Gateway neu aus (Base: kurze
  Unterbrechung, Sessions bleiben; HA: siehe oben). Die alten ConfigMaps und Secrets mit Hash-Suffix bleiben
  liegen; `kubectl rollout undo` braucht sie. Löschen Sie sie erst, wenn Sie nicht mehr zurückrollen wollen.
* Geänderte Profile: Engines mit der alten Spec gehen auf DRAINING (`engine.replaceOnSpecChange`). Neue
  Sessions bekommen neue Engines, bestehende laufen auf der alten weiter, höchstens `engine.drainTimeout`.
* Anderer Namespace: `namespace` im eigenen Overlay setzen (Kustomize benennt den Namespace um und passt die
  RoleBinding an) und `provider.kubernetes.namespace` in der Konfiguration ändern oder weglassen; ohne Angabe
  starten die Engines im Namespace des Gateways.

## Härtung geteilter Engines

Das Gateway authentifiziert, wählt die Engine und setzt Limits durch. **SQL prüft es nicht**: Innerhalb einer
Engine darf jede Session alles, was DuckDB dort erlaubt.

* **Nicht vertrauenswürdige Benutzer bekommen eigene Engines:** `USER` oder `CONNECTION` (am stärksten: der
  Pod endet mit der Session). Erzwingen lässt sich das mit `profiles.<name>.allowedShareLevels` oder
  `overrides[].allowedShareLevels`, z. B.:

  ```yaml
  overrides:
    - groups: [extern]
      defaults: {shareLevel: CONNECTION}
      allowedShareLevels: [CONNECTION]
  ```

* **`SERVER`- und `GROUP`-Engines teilen Datenbankzustand.** Alle Sessions sehen dieselben Tabellen, Views,
  angehängten Datenbanken und in der Engine angelegten Secrets. `SET GLOBAL` wirkt für alle, Dateien unter
  `/tmp` und die Zugangsdaten der Engine (etwa aus `env`) stehen allen offen. Eine teure Query bremst alle,
  Speicherdruck trifft alle.
* **Server-Funktionen:** Jede Session kann mit `quack_stop(…)` den Quack-Server einer Engine für alle anhalten
  oder mit `quack_serve(…)` weitere Server öffnen, und mit `getenv('QUACK_TOKEN')` das Engine-Token lesen. Die
  NetworkPolicy lässt nur das Gateway an Port 9494 der Engines; ein ausgelesenes Token oder ein zusätzlicher
  Server nützt deshalb von außen nichts. Gegen `quack_stop` hilft sie nicht.
* **Gegenmittel im `initSQL`** des Profils (läuft nach den `settings`, bevor der Server startet):
  * `quack_authorization_function`: Die Quack-Extension kann vor der Ausführung von Anfragen eine eigene
    SQL-Funktion zur Autorisierung aufrufen. Damit lassen sich etwa `quack_stop`, `quack_serve` oder
    Dateizugriffe sperren (Signatur: Dokumentation der Quack-Extension).
  * `SET lock_configuration = true` als letzte Anweisung verhindert weitere `SET`s der Sessions, etwa
    `SET GLOBAL threads` oder ein Zurückdrehen von Einstellungen. Clients können sich weiter verbinden und
    abfragen (mit DuckDB 2.0.0 geprüft).
  * `SET enable_external_access = false` gehört **nicht** ins `initSQL`: Die Engine startet dann nicht, weil
    der Entrypoint das Token danach per `getenv()` liest und `quack_serve` selbst Dateizugriff braucht. Nach dem
    Serverstart gesetzt, würde es Datei- und Netzwerkzugriffe und `getenv()` für alle Sessions sperren, ohne den
    Quack-Server zu stören. Dafür bietet das Engine-Image derzeit keinen Haken.
  * `quack_stop(…)` bleibt für jede Session aufrufbar, auch mit gesperrter Konfiguration. Dagegen hilft nur die
    `quack_authorization_function`.
* **Ressourcen:** `resources.limits` und `settings.memory_limit` je Profil begrenzen eine Engine, `limits.*`
  die Zahl der Engines und Sessions.
* **Sandbox:** Wo vorhanden, isoliert eine Sandbox-Runtime die Engines zusätzlich
  (`profiles.<name>.podTemplate.spec.runtimeClassName`, z. B. gVisor).
* **Namespace:** Pod Security `restricted` ist aktiv. Das Gateway darf alle Secrets des Namespace lesen;
  lassen Sie dort nichts anderes laufen.

## Fehlersuche

```sh
kubectl -n quackmux logs deployment/quackmux -f
kubectl -n quackmux get pods -l app.kubernetes.io/component=engine -L quackmux.io/share-level,quackmux.io/profile
kubectl -n quackmux describe pod quack-user-alice-x7k2p
kubectl -n quackmux logs quack-user-alice-x7k2p
kubectl -n quackmux get events --sort-by=.lastTimestamp
```

| Symptom | Ursache und Abhilfe |
|---|---|
| Engine-Pod bleibt `Pending`; Client: `quackmux: could not provide an engine: engine did not become ready in time (3m0s)` | Zu wenig CPU oder Speicher für die Requests des Profils, ResourceQuota, NodeSelector oder Taints passen nicht. `kubectl describe pod` zeigt den Grund in den Events. Requests verkleinern, Nodes ergänzen oder `startupTimeout` erhöhen (etwa wenn der Cluster-Autoscaler erst einen Node startet). |
| `ImagePullBackOff` / `ErrImagePull` | Image-Name oder Tag falsch, Registry nicht erreichbar, private Registry ohne `imagePullSecrets`. Gateway: Deployment patchen; Engines: `profiles.<name>.image` und `imagePullSecrets`. kind: Images mit `kind load docker-image` laden. Fehlgeschlagene Engine-Pods bleiben `engine.failedRetention` (10 min) zur Analyse stehen. |
| Gateway-Log: `launch failed: … violates PodSecurity "restricted…"` | Ein Pod-Template des Profils enthält einen Container ohne gehärteten Security-Context (z. B. einen Sidecar). Template anpassen oder den Namespace bewusst auf `enforce: baseline` stellen. |
| Gateway-Pod in `CrashLoopBackOff` | `kubectl -n quackmux logs deployment/quackmux --previous`: Konfigurationsfehler (unbekanntes Feld), fehlende oder ungültige `users.yaml`, Session-Key kürzer als 16 Bytes. |
| `Authentication failed` | Token unbekannt (in `users.yaml` gehört das sha256 des Tokens ohne `?…`), abgelaufen (`expires`) oder der Benutzer ist `disabled`. Passt der `SCOPE` des Secrets nicht zum Ziel des `ATTACH` (anderer Host oder Port, `localhost` statt `127.0.0.1`), verwendet der Client das Secret nicht. Gateway-Log: `authentication failed` mit Grund; Metrik `quackmux_auth_failures_total`. |
| `quackmux: session closed: idle timeout (6h0m0s). DETACH and ATTACH again to reconnect.` | Die Session hat länger als `session.idleTimeout` keine Query geschickt; Heartbeats zählen nicht. `DETACH` und erneut `ATTACH`, oder `session.idleTimeout` erhöhen (`0s` schaltet ab). Verwandte Gründe: `heartbeat lease expired` (der Client war länger als Lease plus `heartbeatGrace` stumm), `max lifetime`, `engine …` (Engine beendet oder ausgefallen). |
| `Failed to send message` oder SSL-Fehler beim `ATTACH` | HTTPS und HTTP passen nicht zusammen. Ohne `server.tls` spricht das Gateway Plain HTTP; für jeden Host außer `localhost` braucht der Client dann `(DISABLE_SSL true)`. Umgekehrt darf `DISABLE_SSL` nicht gesetzt sein, wenn Gateway oder Load Balancer TLS sprechen (das Gateway antwortet sonst mit HTTP 400). Steht ein L7-Proxy davor, sind auch dessen Timeouts und Body-Limits verdächtig (siehe „Verbinden“). |
| `quackmux: gateway is not ready: not the leader` | Die Verbindung landet direkt beim Follower, etwa durch einen Port-Forward auf einen bestimmten Pod. `svc/quackmux` verwenden. |
| `quackmux: could not provide an engine: engine limit reached …` | `limits.maxEngines`, `maxEnginesPerUser` oder `maxStartingEngines` erreicht; `GET /api/v1/engines` zeigt, wer sie belegt. |
| `quackmux: not permitted: …` | Share-Level, Profil oder Gruppe sind für den Benutzer nicht erlaubt (`allowedShareLevels`, `allowedUsers`, `allowedGroups`, `overrides`). |
