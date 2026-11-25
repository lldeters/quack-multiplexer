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
  * Gateway `ghcr.io/lldeters/quackmux:0.1.0`. Der Entrypoint des Images muss das `quackmux`-Binary sein; das
    Deployment übergibt nur die Argumente `serve -config /etc/quackmux/config.yaml`.
  * Engine `ghcr.io/lldeters/duckdb-engine:2.0.0-0.1.0` (DuckDB 2.0.0 für quackmux 0.1.0) aus [images/engine](../../images/engine),
    je Profil in `config.yaml` eingetragen (`profiles.<name>.image`).
  * Beide Images veröffentlicht der Release-Workflow für amd64 und arm64 auf der GitHub Container Registry
    ([docs/releasing.md](../../docs/releasing.md)). Die Tags sind unveränderlich; ein neues Release ändert die
    Referenzen im Repository.
  * Eigene Registry: das Gateway-Image per `kustomize edit set image ghcr.io/lldeters/quackmux=registry.example.com/quackmux:1.2.3`
    (bzw. `images:` im Overlay), das Engine-Image in `config.yaml`. Private Registries brauchen
    `imagePullSecrets`: für das Gateway per Patch des Deployments, für Engines in `profiles.<name>.imagePullSecrets`.

## Installation

1. Echte Secrets erzeugen (nächster Abschnitt). Die Dateien unter `base/secrets/` sind öffentlich bekannte
   Platzhalter. Solange Session-Key oder Admin-Token Platzhalter sind, startet das Gateway nicht (außer mit
   `auth.mode: none`, etwa im Overlay `dev`). Den Beispiel-Token der Platzhalter-`users.yaml` ignoriert es immer.
   Eine leere Tokendatei (etwa nach einem fehlgeschlagenen `quackmux token > …`) ist ebenfalls ein Startfehler.
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
| ServiceAccount, Role, RoleBinding `quackmux-gateway` | Engine-Pods, ihre Token-Secrets und die Daten-Volumes persistenter Profile verwalten, Events schreiben, Lease (HA) |
| ConfigMap `quackmux-config-<hash>` | `config.yaml`, gemountet als `/etc/quackmux/config.yaml` |
| Secrets `quackmux-session-key-<hash>`, `quackmux-admin-token-<hash>`, `quackmux-users` | `/etc/quackmux/session/key`, `/etc/quackmux/admin/token`, `/etc/quackmux/users/users.yaml` |
| Deployment `quackmux` | Gateway, 1 Replika, Strategie `Recreate` |
| Service `quackmux` | ClusterIP: 9494 (Quack), 9495 (Admin-API, Probes, Metriken) |
| NetworkPolicies | Engines nur vom Gateway auf 9494; Gateway: 9494 von überall, 9495 nur aus dem Namespace |
| PersistentVolumeClaims `quackmux-data-…` | nur mit persistenten Profilen: Daten eines Engine-Schlüssels, angelegt vom Gateway. Sie überleben Engines und Gateway, nicht aber ihren Namespace: `kubectl delete -k` löscht den Namespace samt allen Daten ([Persistente Daten](#persistente-daten)) |

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
quackmux hash-token < token.txt           # sha256 eines bestehenden Tokens; von stdin gelesen,
                                          # erscheint es weder in der Prozessliste noch in der Shell-History
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
9494, `externalTrafficPolicy: Local`). Übernehmen Sie ihn in Ihr Overlay; mit `overlays/ha` gehört
`quackmux.io/leader: "true"` in seinen Selektor, sonst erreicht er auch den Follower (siehe „Hochverfügbarkeit“).
Mit Plain HTTP gehen die Tokens im Klartext über das Netz; außerhalb vertrauenswürdiger Netze gehört TLS davor:

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
  Start; nach einer Erneuerung `kubectl -n quackmux rollout restart deployment/quackmux` (im HA-Modus übergibt
  der Rollout selbst an einen neuen Pod, siehe unten).
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
Benutzer wählen darf, regeln `profiles.<name>.allowedShareLevels`, `allowedUsers`, `allowedGroups`,
`allowedSubdomains` und `overrides` (in der Base: `large` nur mit `user` oder `connection`). Verstöße meldet der
Client als `quackmux: not permitted: …`.

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
curl -s -H "Authorization: Bearer $TOKEN" "http://localhost:9495/api/v1/volumes?owner=alice"
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
| `GET /api/v1/volumes[?owner=…]` | Daten-Volumes persistenter Profile mit Größe, Zustand, letztem Start und der Engine, die sie nutzt (503, wenn diese Replika nicht Leader ist oder noch nicht alle Engines kennt) |
| `DELETE /api/v1/volumes/{name}` | Volume löschen (409, solange eine Engine des Schlüssels existiert oder ein anderer Pod den Claim mountet; 503 wie bei `GET`) |

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

Mit `overlays/ha` zeigt der Service nur auf den Leader; ein `ServiceMonitor` sammelt dann nur dessen Metriken.
Für alle Replikas einen `PodMonitor` verwenden (die Annotationen `prometheus.io/*` wirken ohnehin pro Pod):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: quackmux
  namespace: quackmux
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: quackmux
      app.kubernetes.io/component: gateway
  podMetricsEndpoints:
    - port: admin
      path: /metrics
```

| Metrik | Bedeutung |
|---|---|
| `quackmux_sessions{share_level,profile}` | offene Sessions |
| `quackmux_sessions_opened_total{share_level,profile,adopted}` | geöffnete Sessions (`adopted`: nach einem Neustart übernommen) |
| `quackmux_sessions_closed_total{reason}` | geschlossene Sessions, z. B. `client disconnect`, `idle timeout`, `heartbeat lease expired`, `engine` |
| `quackmux_engines{state,share_level,profile}` | Engines je Zustand (`STARTING`, `READY`, `DRAINING`, `FAILED`, `TERMINATING`, Warm-Pool: `WARM_…`) |
| `quackmux_engine_starts_total{profile,result}`, `quackmux_engine_start_seconds{profile}` | Engine-Starts und ihre Dauer |
| `quackmux_engines_terminated_total{reason}` | beendete Engines (`idle`, `drained`, `failed`, `admin` …) |
| `quackmux_orphan_sweeps_total{result}`, `quackmux_orphaned_connections_closed_total` | nach einem Neustart oder einer Übernahme geprüfte Engines (`ok`; `failed`: Listing oder Heartbeat gescheitert; `skipped`: Engine beendet, Listing vom Autorisierungs-Makro abgelehnt oder Lease nicht frisch) und die verwaisten Verbindungen, die die Engines dabei entfernt haben ([Neustarts und verwaiste Verbindungen](#neustarts-und-verwaiste-verbindungen)) |
| `quackmux_requests_total{type,outcome}`, `quackmux_request_seconds{type}` | Quack-Requests nach Nachrichtentyp |
| `quackmux_bytes_total{direction}` | durchgereichte Bytes (`in`: Client→Engine, `out`: Engine→Client) |
| `quackmux_connect_failures_total{reason}`, `quackmux_auth_failures_total{method}` | gescheiterte Handshakes, fehlgeschlagene Authentifizierungen |
| `quackmux_query_timeouts_total{profile,result}` | Queries, die das Query-Zeitlimit abgebrochen hat (`limits.queryTimeout`, siehe Fehlersuche). `result="failed"`: Das Gateway konnte die Query nicht auf der Engine abbrechen, sie kann dort weiterlaufen; jeder Anstieg verdient einen Alarm. Requests, die das Gateway deshalb beantwortet, zählt `quackmux_requests_total` mit `outcome="query_timeout"`. |
| `quackmux_requests_in_flight`, `quackmux_is_leader` | laufende Requests; 1 auf der Replika, die bedient |
| `quackmux_leader_label`, `quackmux_leader_label_updates_total{action,result}` | nur mit HA: 1, solange der eigene Pod das Label `quackmux.io/leader` trägt (von der API bestätigt); Patches des Labels (`set`, `remove`, `remove_stale`) nach Ergebnis. Alarm (über den `PodMonitor`), wenn `max(quackmux_leader_label)` länger als eine Minute 0 ist oder fehlt: Dann bedient keine Replika, und der Service hat keinen Endpunkt. Ist dabei `max(quackmux_is_leader)` 1, findet der Leader nicht in den Dienst (Label, Engine-Registry, siehe Fehlersuche); ist es 0, führt keine Replika (Lease nicht nutzbar). Quack-Requests, die bei einem Follower ankommen, zählt `quackmux_requests_total` mit `outcome="not_leader"`. |

Das Audit-Log (`audit.enabled`, in der Base an) schreibt jede Query als JSON-Zeile mit Benutzer, Session,
Engine und gekürztem SQL ins Gateway-Log. Statements, die Zugangsdaten enthalten können, stehen dort ohne ihren
Text (`"sql":"[redacted: …]"`, `"redacted":true`): `CREATE SECRET` und alles, was `secret`, `passw`, `pwd`,
`token`, `AccountKey` oder eine SAS-Signatur (`sig=`) enthält, auch in Namen wie `s3_secret_access_key`, sowie URLs
mit Benutzer und Passwort. Das ist eine Heuristik: Behandeln Sie das Audit-Log trotzdem als vertraulich. Bricht das
Query-Zeitlimit eine Query ab, folgt eine Zeile `"msg":"query timeout"` mit derselben `clientQueryId`, dem Limit,
seiner Quelle, der Dauer und dem Ergebnis des Abbruchs (`cancel`: `cancelled` oder `failed`).

```sh
kubectl -n quackmux logs deployment/quackmux | grep '"component":"audit"'
```

Mit `overlays/ha` schreibt jeder Pod, solange er Leader ist, seine eigenen Audit-Zeilen, und
`logs deployment/quackmux` liest nur einen Pod, nicht unbedingt den Leader. Lesen Sie alle Gateway-Pods; die
Zeilen früherer Leader gehen mit deren Pods verloren, dauerhaft hält sie nur eine Log-Sammlung:

```sh
kubectl -n quackmux logs -l app.kubernetes.io/component=gateway --prefix --tail=-1 | grep '"component":"audit"'
```

## Hochverfügbarkeit (`overlays/ha`)

```sh
kubectl apply -k deploy/kubernetes/overlays/ha     # bzw. Ihr Overlay auf dieser Basis
kubectl -n quackmux rollout status deployment/quackmux
```

* Zwei Replikas wählen über die Lease `quackmux-default` einen Leader. **Beide Replikas sind ready**, Quack-Traffic
  bekommt nur der Leader: Sobald er bedienen kann (Lease frisch, Engine-Registry synchronisiert), setzt er an seinem
  Pod das Label `quackmux.io/leader=true`, und der Service `quackmux` wählt zusätzlich dieses Label aus
  (`overlays/ha/service-patch.yaml`). `kubectl rollout status`, `kubectl drain`, das PodDisruptionBudget und
  Health-Checks wie der von Argo CD verhalten sich deshalb wie bei jedem anderen Deployment (`UP-TO-DATE 2`,
  `AVAILABLE 2`).
* Ein Follower meldet sich ready, sobald er übernehmen könnte: Er bewirbt sich um die Lease (`3 × ha.retryPeriod`,
  also 6 s nach dem Start), eine Anfrage an die Lease ist gelungen (die letzte höchstens `2 × ha.leaseDuration`
  her, also 30 s), und die API hat bestätigt, dass sein Pod das Label nicht trägt. Ein Label, das ein früherer
  Container desselben Pods hinterlassen hat, entfernt er dabei. Ein Leader ist erst ready, wenn er bedient:
  Solange er die Lease verlängert, kann kein anderer übernehmen. Bis dahin nennt `/readyz` den Grund.
* **Voraussetzungen:** Das Gateway läuft als Pod und kennt dessen Namen und Namespace aus der Downward-API
  (`POD_NAME`, `POD_NAMESPACE`, wie in `base/deployment.yaml`), und seine Role erlaubt `patch` auf Pods im eigenen
  Namespace (`base/rbac.yaml`). Beim Start fragt es die API außerdem, ob es die Lease lesen und ändern (`get`,
  `update` im Namespace der Lease: `ha.namespace`, sonst der Engine-Namespace), sie anlegen, solange es sie noch
  nicht gibt (`create`; eine Role ohne `create` passt also zu einer vorher angelegten Lease), und die Engine-Pods
  auflisten und beobachten darf (`list`, `watch` im Engine-Namespace). Ohne das könnte es nie bedienen. Verneint
  die API etwas davon, startet das Gateway mit `ha.enabled` nicht und nennt den Grund; ein Rollout mit solchen
  Pods bleibt stehen, die alten bedienen weiter. Kann die API die Frage nicht beantworten, startet es mit einer
  Warnung im Log; ein Follower wird dann trotzdem erst ready, wenn er die Lease lesen konnte. Leader-Election
  außerhalb des Clusters (per Kubeconfig) ist nicht möglich.
* Leader anzeigen: `kubectl -n quackmux get pods -L quackmux.io/leader` oder
  `kubectl -n quackmux get lease quackmux-default -o jsonpath='{.spec.holderIdentity}'` (Metriken
  `quackmux_is_leader`, `quackmux_leader_label`).
* **Logs:** `kubectl -n quackmux logs deployment/quackmux` liest einen beliebigen bereiten Pod, nicht unbedingt den
  Leader. Den zeigt `kubectl -n quackmux logs -l app.kubernetes.io/component=gateway,quackmux.io/leader=true --tail=-1`,
  alle Replikas `kubectl -n quackmux logs -l app.kubernetes.io/component=gateway --prefix --tail=-1`.
* **Eigene Services:** Jeder Service, der Quack-Traffic zum Gateway leitet (etwa die LoadBalancer-Variante aus
  `base/service.yaml`), braucht im HA-Betrieb ebenfalls `quackmux.io/leader: "true"` im Selektor. Sonst landen
  Requests auch beim Follower. Der führt sie nicht aus, sondern antwortet mit dem Quack-Fehler
  `quackmux: gateway is not ready: not the leader`, zählt sie als `quackmux_requests_total{outcome="not_leader"}`
  und warnt im Log (höchstens einmal pro Minute).
* **Geordnetes Herunterfahren** (Pod löschen, Rollout, Drain): Der Leader nimmt keine neuen Requests mehr an,
  schließt ruhende Keep-Alive-Verbindungen, entfernt sein Label, gibt die Lease frei und lässt danach seine
  laufenden Requests auslaufen. Der wartende Follower übernimmt innerhalb von Sekunden, synchronisiert die
  Engine-Registry und setzt sein Label. In kind war der Service dabei 0,5–3,6 s ohne bereiten Endpunkt; meist
  wartet der Follower auf seinen nächsten Versuch (alle `ha.retryPeriod` plus Jitter, bis 4,4 s). Ein frisch
  gestarteter Pod bewirbt sich erst nach `3 × ha.retryPeriod`. Bis kube-proxy seine Regeln erneuert hat
  (höchstens einmal pro Sekunde), schickt er neue Verbindungen noch zum alten Pod. Der alte Leader beantwortet sie
  deshalb noch `ha.deregistrationDelay` (Default 3 s, bei kurzem `server.shutdownTimeout` dessen Hälfte; ein
  gesetzter Wert muss kürzer sein als `server.shutdownTimeout`) nach dem
  Entfernen des Labels mit einem Fehler; ein beendeter Pod ließe sie bis zu zwei Minuten im Verbindungsaufbau
  hängen. Queries, die beim Herunterfahren noch laufen, bekommen kurz vor `server.shutdownTimeout` einen Fehler und
  werden auf der Engine abgebrochen, damit libcurl sie nicht unbemerkt ein zweites Mal sendet.
* **Ausfall des Leaders** (Node weg, Prozess hängt): Der Follower übernimmt, wenn die Lease abgelaufen ist
  (`ha.leaseDuration`, 15 s, plus Retry; in kind gemessen: 16–20 s). Er entfernt das Label vom Pod des alten
  Leaders, damit der Service diesen nicht weiter nennt, auch wenn dessen Node nicht mehr antwortet und der Pod
  noch bis zu 50 s als ready gilt (`node-monitor-grace-period`). Kann ein Leader die Lease nicht verlängern,
  beendet sich sein Prozess und startet als Follower neu. Wacht ein eingefrorener Leader wieder auf, tritt er
  zurück, ohne Sessions zu schließen, die Lease des neuen Leaders anzutasten oder sein Label wieder zu setzen;
  bis zum Neustart meldet er sich nicht ready, danach ist er ein bereiter Follower. Clients, deren Verbindung
  noch zum eingefrorenen Pod führt, warten, bis er aufwacht oder neu gestartet wird.
* In allen Fällen findet der neue Leader die Engines wieder und übernimmt die Sessions mit deren nächstem
  Heartbeat oder Request. Die Sessions überleben. In der kurzen Lücke ohne bereiten Endpunkt scheitern Requests
  mit „Could not connect to server“; sie laufen nicht und können wiederholt werden.
* Geprüft mit [test/e2e/kind/ha.sh](../../test/e2e/kind/ha.sh): Übergabe, eingefrorener Leader, Rollout,
  PodDisruptionBudget und ein Rollout von Pods, die die Lease nicht nutzen dürfen, mit angehängten
  DuckDB-2.0-Clients.
* Eine bevorzugte `podAntiAffinity` verteilt die Replikas auf verschiedene Nodes.
* `overlays/ha/config.yaml` ersetzt `base/config.yaml` vollständig. Halten Sie beide Dateien bis auf den
  Abschnitt `ha` gleich.

**Rollout (neues Image, neue Konfiguration):** `kubectl apply -k …` genügt, `kubectl rollout status` meldet Erfolg
(in kind nach 13–17 s). Mit `maxSurge: 1` und `maxUnavailable: 0` startet Kubernetes zuerst einen neuen Pod
und wartet, bis er als Follower ready ist. Dann entfernt es den alten Follower: Der Leader gibt seinem Pod höhere
Löschkosten (`controller.kubernetes.io/pod-deletion-cost`; ein Wert aus dem Pod-Template wird überschrieben),
deshalb geht er nicht zuerst. Ist auch der zweite neue Pod ready, endet der alte Leader und übergibt an einen der
neuen Pods. Die Lease wechselt je Rollout genau einmal (solange der Rollout die Lease nicht selbst ändert, siehe
unten); es sind immer mindestens zwei Replikas ready, kurzzeitig laufen drei Pods. Fehlt im Cluster der Platz für
einen dritten Gateway-Pod, setzen Sie im eigenen Overlay `maxSurge: 0` und `maxUnavailable: 1`: Dann geht zuerst
der Follower, und bis sein Ersatz ready ist, gibt es keinen Standby.

`kubectl rollout status` meldet Erfolg, sobald der alte Leader zu enden beginnt, also vor der Übergabe: Erst dann
entfernt er sein Label und gibt die Lease frei, und ein neuer Pod übernimmt (bis etwa 4,4 s ohne Endpunkt, in kind
0,5–3,1 s). Seine laufenden Requests lässt der alte Leader danach noch auslaufen (höchstens
`server.shutdownTimeout`). Wer gleich nach dem Rollout testet (Smoke-Test, PostSync-Hook von Argo CD), wartet
zuerst auf einen bereiten Endpunkt:

```sh
until kubectl -n quackmux get endpointslices -l kubernetes.io/service-name=quackmux \
    -o go-template='{{range .items}}{{range .endpoints}}{{if .conditions.ready}}{{index .addresses 0}}{{"\n"}}{{end}}{{end}}{{end}}' |
    grep -q .; do sleep 1; done
```

**Lease nicht per Rollout ändern:** Die Lease ergibt sich aus `ha.namespace` (sonst dem Engine-Namespace, also
`provider.kubernetes.namespace` oder dem Namespace des Gateways) und `ha.leaseName` (sonst `quackmux-` und
`provider.kubernetes.instance`). Ändert ein Rollout eines davon, bewirbt sich der erste neue Pod um eine andere,
freie Lease und führt nach `3 × ha.retryPeriod` neben dem alten Leader; beide setzen ihr Label und bedienen.
Solche Änderungen nur wie beim Umstieg von der Base: erst auf 0 skalieren, dann anwenden.

**Node-Wartung:** `kubectl drain` braucht keine Vorbereitung. Das PodDisruptionBudget (`minAvailable: 1`) lässt
die Räumung des Leaders zu, solange der Follower ready ist; der Leader übergibt dann wie beim Löschen. Die letzte
bereite Replika räumt es nicht: `kubectl drain` wartet, bis der Ersatz-Pod ready ist. Nicht bereite Pods dürfen
immer gehen.

**Umstieg von der Base:** Zuerst das einzelne Gateway stoppen, damit nie ein Gateway ohne Leader-Election
neben einem Leader läuft:

```sh
kubectl -n quackmux scale deployment/quackmux --replicas=0
kubectl apply -k deploy/kubernetes/overlays/ha
```

**Umstieg von einem Stand ohne Leader-Label** (frühere Fassungen von `overlays/ha`, in denen nur der Leader ready
ist): Ab dem `kubectl apply` wählt der Service nur noch Pods mit dem Label aus, und die alten Pods setzen es nie.
Ein Rollout ließe den Service deshalb bis zu seinem Ende ohne Endpunkt (in kind 15–19 s). Gehen Sie wie beim
Umstieg von der Base vor (erst auf 0 skalieren, dann anwenden): Dann fehlt der Endpunkt nur, bis ein neuer Pod
führt (Start und `3 × ha.retryPeriod`). Ergänzen Sie im selben Schritt den Selektor eigener Services.

**Zurückrollen auf einen Stand ohne Leader-Label:** das alte Overlay anwenden (`kubectl apply -k` mit dem alten
Stand), wieder erst nach dem Skalieren auf 0. Das entfernt auch den Selektor `quackmux.io/leader` aus dem Service.
`kubectl rollout undo` oder `kubectl set image` genügen nicht: Der Service behielte den Selektor, und sobald ein
alter Pod führt, hätte er keinen Endpunkt mehr. Ein älteres Gateway lehnt außerdem `ha.deregistrationDelay` in der
Konfiguration ab.

## Entwicklung mit kind (`overlays/dev`)

`auth.mode: none`, Admin-API ohne Token, kleine Ressourcen (Gateway bis 512 MiB, Engines 100m/256 MiB
angefordert, bis 1 CPU/1 GiB), höchstens 8 Engines, Debug-Logs im Textformat. Das Profil `persistent`
(`TOKEN 'dev?user=alice&profile=persistent'`) gibt jedem Benutzer ein Volume von 1 GiB aus der
Default-StorageClass von kind. **Nur auf dem eigenen Rechner verwenden:** Jeder Client darf sich jeden
Benutzernamen aussuchen.

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
* Vorher prüfen: `quackmux validate -config meine-umgebung/config.yaml`. Unbekannte Felder sind Fehler. Ohne
  Cluster prüft `validate` auch, was das Gateway beim Start ablehnen würde: Provider-Einstellungen, jedes Profil
  einmal als Pod gerendert, Lease-Name und Timings der Leader-Election. Token- und Secret-Dateien liest es nicht.
* `kubectl apply -k …` erzeugt eine neue ConfigMap (neuer Hash) und rollt das Gateway neu aus (Base: kurze
  Unterbrechung, Sessions bleiben; HA: siehe oben). Die alten ConfigMaps und Secrets mit Hash-Suffix bleiben
  liegen; `kubectl rollout undo` braucht sie. Löschen Sie sie erst, wenn Sie nicht mehr zurückrollen wollen.
* Mit `overlays/ha` ändern `ha.namespace`, `ha.leaseName`, `provider.kubernetes.instance` und, ohne
  `ha.namespace`, `provider.kubernetes.namespace` die Lease: nicht per Rollout, sondern wie beim Umstieg (siehe
  „Hochverfügbarkeit“, „Lease nicht per Rollout ändern“).
* Mit `auth.mode: jwt` kennt das Gateway nach einem Rollout (und jedem Failover) die Gruppen der übernommenen
  Sessions nicht, sie stehen nur im Token des `ATTACH`. Overrides nach `groups`, die `queryTimeout` setzen, gelten
  für diese Sessions dann nicht mehr, ihre Queries bekommen das Limit des nächsten passenden Overrides, des Profils
  oder `limits.queryTimeout`; `validate` warnt davor. Mit `static` und `none` liest das Gateway die Gruppen aus der
  aktuellen Benutzerdatei bzw. `auth.none.defaultGroups`; Änderungen dort gelten dann auch für offene Sessions.
* Geänderte Profile: Engines mit der alten Spec gehen auf DRAINING (`engine.replaceOnSpecChange`). Neue
  Sessions bekommen neue Engines, bestehende laufen auf der alten weiter, höchstens `engine.drainTimeout`.
  Persistente Profile: Die alte Engine nimmt weiter Sessions an, bis sie keine mehr hat (spätestens
  `engine.drainTimeout`); erst dann startet die neue ([Persistente Daten](#persistente-daten)).
* Anderer Namespace: `namespace` im eigenen Overlay setzen (Kustomize benennt den Namespace um und passt die
  RoleBinding an) und `provider.kubernetes.namespace` in der Konfiguration ändern oder weglassen; ohne Angabe
  starten die Engines im Namespace des Gateways. Die Daten-Volumes persistenter Profile liegen im Namespace der
  Engines: Im neuen bekommt jeder Schlüssel ein neues, leeres Volume, die alten bleiben im alten Namespace
  ([Persistente Daten](#persistente-daten)).

## Neustarts und verwaiste Verbindungen

Ein Neustart des Gateways (Rollout, Absturz, mit `overlays/ha` die Übernahme durch einen Follower) beendet weder
Engines noch Sessions: Die Clients schicken weiter Heartbeats, und das Gateway übernimmt ihre Sessions mit dem
ersten Request ([docs/design.md](../../docs/design.md), Abschnitt 6). Ein Client, der während der Downtime stirbt,
kommt nie wieder. Seine Verbindung bliebe auf der Engine offen, denn der Quack-Server prüft eine Lease erst, wenn
ein Request für die Verbindung kommt: auf einer geteilten Engine, die andere Sessions am Leben halten, bis zu
ihrem Ende, samt temporären Tabellen, offenen Transaktionen und einer Query, die vielleicht noch läuft.

Deshalb räumt das Gateway nach jeder Übernahme auf (`engine.closeOrphanedConnections`, Default an, außer beim
`static`-Provider):

1. Gleich nach dem Start listet es die Verbindungen jeder wiedergefundenen Engine der Share-Levels `USER`,
   `GROUP` und `SERVER` (eine `CONNECTION`-Engine endet mit ihrer Session) über eine eigene Verbindung mit dem
   Engine-Token: `SELECT connection_id FROM quack_active_connections()`.
2. Es wartet, bis die Engine-Lease jeder Verbindung abgelaufen ist, die nur der vorige Prozess am Leben hielt:
   `session.backendHeartbeatTimeout` (Default 1 h) plus `session.heartbeatGrace`, nach der Uhr. Diese Lease
   verlangt das Gateway bei jedem Handshake mit einer Engine und erneuert sie nur, solange die Session offen ist.
3. Dann schickt es jeder gelisteten Verbindung, die zu keiner offenen Session gehört, einen Heartbeat. Ist ihre
   Lease abgelaufen, entfernt die Engine sie und bricht eine Query ab, die darauf noch läuft. Läuft die Lease
   noch, verlängert die Engine sie nur. Mit HA tut das nur ein Leader mit frischer Lease.

So nimmt das Gateway keinem Client eine Verbindung, die er noch benutzen könnte: Eine Verbindung mit abgelaufener
Lease lehnt die Engine bei der nächsten Nachricht ohnehin ab, ob aufgeräumt wird oder nicht. Wer nach einem
Ausfall des Gateways lange fernbleibt, wird übernommen, solange die Lease seiner Verbindung läuft (nach dem Ausfall
noch mindestens zwei Drittel von `session.backendHeartbeatTimeout`, mit den Defaults rund 40 min) und die Engine
so lange läuft, etwa wenn sein Heartbeat an der Keep-Alive-Verbindung zu einem verlorenen Node hängt, bis TCP
aufgibt (unter Linux rund 15 min), oder an einem eingefrorenen Leader, bis der neu startet. Eine Engine, auf der das
neue Gateway keine Session übernommen hat, endet allerdings schon nach `engine.idleTimeout`. Verbindungen, die andere am Leben halten (ein zweites
Gateway an derselben Engine, ein Client, der die Engine direkt erreicht), bleiben unberührt.

Grenzen:

* Verwaiste Verbindungen bleiben bis zum Aufräumen bestehen, mit den Defaults also gut eine Stunde lang. Ein
  kürzerer `session.backendHeartbeatTimeout` räumt früher auf, verkürzt aber auch die Zeit, in der ein Client nach
  einem Ausfall zurück sein muss: Kommt er nach Ablauf der Lease, lehnt die Engine seine Verbindung ab, mit oder
  ohne Aufräumen (`quackmux: session closed: the engine no longer knows this connection. DETACH and ATTACH again
  to reconnect.`).
* Hat der vorige Prozess eine längere `session.backendHeartbeatTimeout` verwendet, läuft die Lease mancher
  verwaisten Verbindung beim Heartbeat noch; die Engine verlängert sie, und sie bleibt bis zum Aufräumen nach dem
  nächsten Neustart.
* Startet das Gateway vor dem Aufräumen erneut, beginnt die Wartezeit von vorn.
* DuckDB v2.0.0 listet höchstens 2048 Verbindungen je Engine; darüber scheitert `quack_active_connections()`, und
  das Gateway räumt die Engine nicht auf (Warnung im Log).
* Eine gehärtete Engine mit `quackmux_authorize` lehnt das Listing ab, und ihre verwaisten Verbindungen bleiben.
  Die Variante `quackmux_authorize_listing` lässt es durch, gibt damit aber jeder Session die Verbindungs-IDs der
  Engine ([Gehärtete geteilte Engines](#gehärtete-geteilte-engines)).
* Engines des `static`-Providers gehören nicht dem Gateway; dort räumt es nur mit
  `engine.closeOrphanedConnections: true` auf. Verbindungen, die andere Gateways oder Clients am Leben halten,
  lässt es auch dann stehen.
* Ältere Gateways kennen `engine.closeOrphanedConnections` nicht und lehnen eine Konfiguration damit ab: Vor einem
  Rollback des Gateways muss das Feld heraus.

Das Log zeigt je Engine `closed orphaned connections` mit `closed` (von der Engine entfernt), `kept` (Lease lief
noch) und `adopted` (Verbindungen offener Sessions, nicht angefasst); die Metriken stehen unter
[Metriken](#metriken).

## Persistente Daten

Engines halten ihren Zustand standardmäßig im Speicher (`database: ":memory:"`) und verlieren ihn mit dem Pod.
Für Daten, die bleiben sollen, gibt es drei Wege.

### Ein Volume pro Engine-Schlüssel (`persistence`)

Mit `profiles.<name>.persistence` bekommt jeder Engine-Schlüssel des Profils ein eigenes PersistentVolumeClaim:
bei `USER` jeder Benutzer, bei `GROUP` jede Gruppe, bei `SERVER` das ganze Profil, jeweils pro Subdomain. Das
Gateway legt den Claim beim ersten `ATTACH` an, bevor es den Pod startet, und mountet ihn unter `/data`. Die
Datenbank ist dann `/data/engine.duckdb`.

```yaml
profiles:
  personal:
    description: "Eigene Datenbank pro Benutzer"
    allowedShareLevels: [USER]
    allowedSubdomains: [default]
    persistence:
      size: 10Gi
      # storageClassName: fast-ssd      # ohne Angabe: die Default-StorageClass des Clusters
      # accessMode: ReadWriteOncePod    # Default; ReadWriteOnce, wenn die StorageClass kein RWOP kann
      # mountPath: /data
      # labels: {}                      # zusätzlich am Claim, etwa für Backup-Werkzeuge
      # annotations: {}
```

```sql
CREATE SECRET (TYPE quack, TOKEN 'mein-token?profile=personal', SCOPE 'quack:quackmux.example.com:9494');
ATTACH 'quack:quackmux.example.com:9494' AS meine (DISABLE_SSL true);
CREATE TABLE meine.notizen AS SELECT 'bleibt erhalten' AS text;
```

Regeln, die `quackmux validate` und der Start des Gateways prüfen:

* nur mit `provider.type: kubernetes`;
* `allowedShareLevels` muss gesetzt sein und darf `CONNECTION` nicht enthalten: Jede Session bekäme ein eigenes
  Volume, das nie wieder benutzt würde;
* `allowedSubdomains` muss gesetzt sein. Jede Subdomain bekommt eigene Volumes; ohne Liste könnte ein Benutzer
  beliebig viele anlegen. Mehrere Datenbanken pro Benutzer gehören als Dateien auf sein eines Volume. Die Liste
  wirkt auch ohne `persistence`: Andere Subdomains lehnt das Gateway mit `quackmux: not permitted: …` ab;
* `pool.size: 1` und `warmPool.size: 0`;
* `size` ist Pflicht; `accessMode` ist `ReadWriteOncePod` (Default) oder `ReadWriteOnce`; `mountPath` ist ein
  absoluter Pfad außer `/` und `/tmp`;
* das `podTemplate` darf kein Volume `quackmux-data` definieren, im Engine-Container nichts auf oder unter
  `mountPath` mounten (ein Mount darunter verdeckt einen Teil des Volumes) und kein `spec.nodeName` setzen (das
  umgeht den Scheduler und damit die ReadWriteOncePod-Prüfung). Init-Container und Sidecars des Templates dürfen
  das Volume `quackmux-data` über seinen Namen einbinden;
* ist das Profil ein Default-Profil (`defaults.profile` oder `overrides[].defaults.profile`), müssen der
  jeweilige Default-Share-Level in `allowedShareLevels` und die Default-Subdomain in `allowedSubdomains` stehen;
  sonst lehnte das Gateway jede Session ohne diese Optionen ab.

Ein explizites `database` gilt weiter, auch `":memory:"` (etwa mit `ATTACH '/data/…'` im `initSQL`). Liegt es nicht
unter `mountPath`, warnen `quackmux validate` und der Start, es sei denn, `initSQL` nennt den `mountPath`: Die
Tabellen gingen sonst mit der Engine verloren. Ebenso warnen sie bei `auth.mode: none` (siehe unten).

**Nie zwei Engines auf denselben Daten.** Eine DuckDB-Datei kann nur ein Prozess schreibend öffnen. Das Gateway
startet für einen Schlüssel deshalb erst dann eine neue Engine, wenn der Pod der vorigen aus der Kubernetes-API
verschwunden ist; dann sind seine Container beendet und seine Volumes ausgehängt (wie bei einem StatefulSet).

* Ändert sich das Profil (`engine.replaceOnSpecChange`) oder erreicht die Engine `engine.maxLifetime`, nimmt sie
  weiter Sessions an, auch neue: Keine andere Engine darf die Daten öffnen. Die Admin-API zeigt dann
  `replacement pending: …`. Sobald sie keine Session mehr hat, beendet das Gateway sie, und die nächste Session
  startet die neue Engine. Nach einem Neustart des Gateways, also auch nach jeder Profiländerung, geschieht das
  frühestens nach `engine.recoveryGrace`. Spätestens `engine.drainTimeout`, nachdem die Ersetzung fällig wurde,
  schließt das Gateway die verbliebenen Sessions; deren Clients müssen neu attachen. Den Zeitpunkt vermerkt es am
  Pod (Annotation `quackmux.io/replace-due`): Neustarts und Failover des Gateways verschieben die Frist nicht.
* Ein `ATTACH`, das in diese Lücke fällt, wartet, bis die alte Engine weg ist (Checkpoint und Stopp, höchstens
  die Grace Period), und dann auf den Start der neuen. Beides zusammen zählt gegen `engine.startupTimeout`. Endet
  die alte Engine in dieser Zeit nicht, meldet der Client
  `quackmux: could not provide an engine: the previous engine of this key still holds its data volume (engine …)`.
  Das gilt auch nach `POST /api/v1/engines/{id}/drain`, bis die gedrainte Engine endet.
* Eine ausgefallene Engine (`FAILED`) beendet das Gateway sofort, wenn eine neue Session den Schlüssel braucht.
  Sonst bleibt ihr Pod wie bisher `engine.failedRetention` lang zur Analyse stehen.
* Mit Leader-Election startet nur ein Gateway mit frischer Lease eine Engine für einen solchen Schlüssel, und nur
  dieses beendet dafür eine alte. Es prüft die Lease beim `ATTACH`, vor dem Anlegen des Pods und noch einmal in dem
  Moment, in dem sein API-Client den Request abschickt, also nach dem clientseitigen Rate-Limit
  (`provider.kubernetes.qps` und `burst`), das Requests bei vielen gleichzeitigen Starts sekundenlang zurückhalten
  kann. Die Leader-Election hat
  einen eigenen API-Client mit eigenem Rate-Limit, damit solche Starts die Erneuerung der Lease nicht verzögern.
  Ein Leader, der nach einem Einfrieren aufwacht, lässt die Engines in Ruhe; ein `ATTACH`, das ihn bis zu seinem
  Neustart noch erreicht, scheitert (etwa mit `… this gateway does not hold a fresh leader lease`) und kann
  wiederholt werden.
* Direkt vor dem Anlegen des Pods fragt das Gateway außerdem die Kubernetes-API, ob es schon einen Pod des
  Schlüssels gibt, auch einen, den es nicht kennt: etwa einen, dessen Token-Secret beim Start des Gateways nicht
  lesbar war (das Gateway übernimmt ihn, sobald es das Token lesen kann), oder einen eines anderen
  Gateway-Prozesses. Dann startet es keine Engine, und der Client bekommt
  `… the previous engine of this key still holds its data volume (engine pod … exists …)`.
* `ReadWriteOncePod` sichert zusätzlich ab: Der Scheduler plant keinen zweiten Pod mit demselben Claim ein,
  solange der erste läuft oder herunterfährt (in kind geprüft). `ReadWriteOnce` erlaubt dagegen mehrere Pods auf
  demselben Node; dann schützen nur das Gateway und die Dateisperre von DuckDB („Could not set lock on file“).
* Grenzen:
  * Ein erzwungenes Löschen (`kubectl delete pod --force --grace-period=0`) entfernt den Pod aus der API, bevor
    sein Container beendet ist; weder das Gateway noch der Scheduler sehen ihn danach.
  * Fällt ein Node aus, bleibt der Pod `Terminating`, bis der Node zurückkommt, der Pod erzwungen gelöscht oder
    der Node mit dem Taint `node.kubernetes.io/out-of-service` markiert wird; so lange bekommt der Schlüssel keine
    Engine. Mit lokalem Storage (etwa local-path) liegen die Daten ohnehin auf diesem Node.
  * Mit HA bleibt die Zeit, die die API für einen schon gesendeten Request braucht: Friert der Leader genau dann
    ein, entsteht sein Pod trotzdem. Ein anderer Leader übernimmt frühestens `ha.leaseDuration` nach der letzten
    Erneuerung, frisch ist die Lease nur `ha.renewDeadline` lang. Erst wenn die API für den Request länger braucht
    als die Differenz (mit den Defaults 5 s), sieht der neue Leader den Pod bei seiner Prüfung nicht; dann
    schützen ReadWriteOncePod und die Dateisperre von DuckDB.
  * Ein eigener Scheduler (`podTemplate.spec.schedulerName`) prüft ReadWriteOncePod womöglich nicht (im
    kube-scheduler tut es das Plugin `VolumeRestrictions`); dann schützen nur das Gateway und die Dateisperre.
* Wird `persistence` für ein bestehendes Profil eingeschaltet, nehmen dessen laufende Engines (ohne Volume) keine
  neuen Sessions mehr an und gehen auf DRAINING, auch mit `engine.replaceOnSpecChange: false`; die nächste
  Session eines Schlüssels bekommt sofort eine Engine mit Volume. Die alten Engines enden mit ihrer letzten
  Session (spätestens nach `engine.drainTimeout`); ihre Daten lagen nur im Speicher. Daten aus einem PVC im
  `podTemplate` übernehmen: siehe [Umstieg](#umstieg-von-einem-pvc-im-podtemplate).

**Lebenszyklus der Volumes.** Die Claims haben keine Owner-Reference. Sie bleiben, wenn die Engine endet, das
Profil entfernt oder das Gateway neu ausgerollt wird. Das Gateway löscht sie nie von sich aus.

* Name: `quackmux-data-<level>-<owner>-<subdomain>-<profil>-<hash>`, gekürzt auf ein DNS-Label (63 Zeichen); der
  Hash über `provider.kubernetes.instance` und den vollständigen Schlüssel macht ihn eindeutig. Er hängt von nichts
  anderem ab. Ein umbenanntes Profil, eine andere Subdomain oder Instanz und ein anderer Namespace der Engines
  ergeben ein neues, leeres Volume.
* Labels: `app.kubernetes.io/managed-by=quackmux`, `app.kubernetes.io/component=engine-data`,
  `quackmux.io/instance`, `quackmux.io/key-hash` (wie am Pod), `quackmux.io/share-level`, `quackmux.io/profile`.
  Annotationen: `quackmux.io/key` (der Schlüssel), `quackmux.io/user` (wessen Session ihn angelegt hat),
  `quackmux.io/last-launch` (letzter Start einer Engine damit).
* Einen Claim mit dem richtigen Namen, aber fremden Labels oder anderem `quackmux.io/key` mountet das Gateway nie;
  der Start scheitert mit `refusing to mount a claim that does not belong to the engine key`.
* `GET /api/v1/volumes[?owner=…]` listet die Volumes mit Größe, Zustand, letztem Start und der Engine, die sie
  gerade nutzt. `DELETE /api/v1/volumes/{name}` löscht eines, solange keine Engine des Schlüssels existiert und
  kein anderer laufender Pod den Claim mountet (sonst 409; vorher `DELETE /api/v1/engines/{id}`). Das prüft das
  Gateway in der Kubernetes-API, also auch für Pods, die es nicht kennt (etwa einen Debug-Pod). Nur der Leader
  listet und löscht, und erst wenn er alle Engines kennt (sonst 503). Mit `reclaimPolicy: Delete` der StorageClass
  sind die Daten danach weg.
* `kubectl delete pvc` prüft dagegen nichts: Nutzt ein Pod den Claim, löscht Kubernetes ihn, sobald der Pod endet
  (Finalizer `kubernetes.io/pvc-protection`), samt allem, was die Engine bis dahin geschrieben hat. Besser die
  Admin-API.
* `size`, `storageClassName`, `accessMode`, `labels` und `annotations` gelten nur für neue Claims; bestehende
  behalten ihre Werte. Vergrößern: `kubectl patch pvc …` mit einer StorageClass, die `allowVolumeExpansion` erlaubt.
  Bleibt ein Claim wegen seiner StorageClass oder seines Access-Modes `Pending`, hilft ein korrigiertes Profil
  allein nicht: Engine beenden (`DELETE /api/v1/engines/{id}`), den Claim löschen (`DELETE /api/v1/volumes/{name}`;
  ein nie gebundener Claim enthält keine Daten) und neu attachen. Startet eine Engine mit einem Claim, dessen
  Access-Mode oder (ausdrücklich gesetzte) StorageClass vom Profil abweicht, warnt das Gateway-Log.
* Ohne Authentifizierung (`auth.mode: none`) wählt jeder Client seinen Benutzernamen selbst: Er kann das Volume
  jedes Benutzers öffnen und mit erfundenen Namen beliebig viele anlegen. Persistente Profile gehören hinter
  `auth.mode: static` oder `jwt`; das Dev-Overlay verzichtet darauf nur zum Testen.
* Plattenplatz begrenzen: Eine ResourceQuota im Namespace (`count/persistentvolumeclaims`, `requests.storage`)
  deckelt alle Volumes. Scheitert das Anlegen an ihr, meldet der Client `… exceeded quota …`.
* Bei einer Datei-Datenbank lagert DuckDB Zwischenergebnisse neben der Datei aus (`engine.duckdb.tmp`, also auf dem
  Volume). Mit `settings.temp_directory: /tmp/duckdb-spill` landen sie im `emptyDir` des Pods.
* Beim Beenden macht der Entrypoint einen `FORCE CHECKPOINT`, höchstens `QUACKMUX_STOP_TIMEOUT` lang (Grace Period
  minus 5 s). Große Datenbanken brauchen eine längere `provider.kubernetes.deleteGracePeriod`; sonst spielt die
  nächste Engine das WAL beim Start ein, was gegen `startupTimeout` zählt. Das Bereitstellen eines neuen Volumes
  (Cloud-Disks: oft 10–60 s) zählt ebenfalls dagegen.
* Backups: Volume-Snapshots der StorageClass oder Werkzeuge wie Velero über die Labels; deren Annotationen setzt
  `persistence.annotations`.

**Achtung beim Deinstallieren:** Die Claims liegen im Namespace der Engines und gehen mit ihm.
`kubectl delete -k deploy/kubernetes/overlays/…` löscht den Namespace `quackmux` (base, dev und ha legen ihn an)
und damit alle Claims; mit `reclaimPolicy: Delete` der StorageClass (der Default bei kind und den meisten Clouds)
sind die Daten weg. Für persistente Profile:

* eine StorageClass mit `reclaimPolicy: Retain` verwenden (das PersistentVolume bleibt dann auch nach dem Löschen
  des Claims, die Daten lassen sich von Hand retten) und Backups einrichten (siehe oben);
* zum Deinstallieren nur das Gateway entfernen und den Namespace mit den Claims stehen lassen:

  ```sh
  kubectl -n quackmux delete deployment quackmux
  kubectl -n quackmux delete pod -l app.kubernetes.io/component=engine
  kubectl -n quackmux delete -l app.kubernetes.io/part-of=quackmux \
    service,serviceaccount,role,rolebinding,networkpolicy,poddisruptionbudget,configmap,secret
  ```

  Ein späteres `kubectl apply -k` findet die Claims über ihre Namen wieder. Alternativ ein eigenes Overlay ohne
  `namespace.yaml`, dessen Engines in einem Namespace laufen, den die Kustomization nicht verwaltet.

#### Umstieg von einem PVC im `podTemplate`

Frühere Versionen dieser Anleitung empfahlen für eine einzige schreibbare Datei ein eigenes PVC im `podTemplate`
eines `SERVER`-Profils mit genau einer Engine. Mit `persistence` bekommt der Schlüssel ein neues, leeres Volume. Die
alte Datei übernehmen, während keine Clients arbeiten:

1. Im Profil das Volume und seinen Mount aus dem `podTemplate` entfernen, `allowedShareLevels: [SERVER]`,
   `allowedSubdomains: [default]` und `persistence` ergänzen, `database` weglassen (dann `/data/engine.duckdb`) und
   ausrollen.
2. Einmal attachen: Das Gateway legt den Claim an; `GET /api/v1/volumes` zeigt seinen Namen. Detachen und alle
   Engines des Profils beenden (`GET /api/v1/engines`, `DELETE /api/v1/engines/{id}`), auch die alte mit dem
   alten PVC.
3. Einen Hilfs-Pod starten, der das alte PVC und den neuen Claim mountet (etwa unter `/alt` und `/neu`; Pod
   Security `restricted` verlangt einen gehärteten Security-Context), und kopieren:
   `cp /alt/<datei>.duckdb /neu/engine.duckdb`, eine `<datei>.duckdb.wal` daneben als `/neu/engine.duckdb.wal`.
   Der Hilfs-Pod läuft dabei als Benutzer und Gruppe 1000 wie die Engine (`runAsUser: 1000`, `runAsGroup: 1000`);
   sonst gehören die Kopien einem anderen Benutzer, und die Engine kann sie nicht schreiben (dann
   `chown 1000:1000 /neu/engine.duckdb*`).
4. Den Hilfs-Pod löschen, dann attachen. Solange er den Claim mountet, plant der Scheduler mit `ReadWriteOncePod`
   keine Engine des Schlüssels ein; mit `ReadWriteOnce` könnte eine starten, deshalb bis hierher keine Clients.

### Gemeinsame Daten read-only anhängen

Ein PVC im `podTemplate` ist **kein** Speicher pro Benutzer: Das Gateway rendert das Template für jede Engine des
Profils gleich, alle Engines des Profils mounten also dasselbe Volume (jeder Benutzer, jede Subdomain, Pool- und
Ersatz-Engines). Eine DuckDB-Datei darauf kann nur eine davon schreibend öffnen; jede weitere scheitert beim Start
(`Could not set lock on file`), auf Netzwerk-Dateisystemen ohne verlässliche Locks droht Datenverlust. Das taugt
also nur für Daten, die gelesen werden: PVC (`ReadOnlyMany`) im Template und im `initSQL`
`ATTACH '/data/lake.duckdb' AS lake (READ_ONLY)`. Das verträgt beliebig viele Engines.

### Schreibbare Daten in einem externen Katalog

Etwa DuckLake mit Postgres-Katalog und Objektspeicher (Beispiel in
[images/engine/README.md](../../images/engine/README.md)). Mehrere Engines können so dieselben Tabellen schreiben.

Eine einzige schreibbare Datei für alle: ein Profil mit `allowedShareLevels: [SERVER]`,
`allowedSubdomains: [default]` und `persistence` (Umstieg von einem PVC im `podTemplate`: siehe
[oben](#umstieg-von-einem-pvc-im-podtemplate)).

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
* **Server-Funktionen:** Ohne Härtung kann jede Session mit `quack_stop(…)` den Quack-Server einer Engine für alle
  anhalten, mit `quack_serve(…)` weitere Server öffnen und mit `getenv('QUACK_TOKEN')` das Engine-Token lesen. Die
  NetworkPolicy lässt nur das Gateway an Port 9494 der Engines; ein ausgelesenes Token oder ein zusätzlicher
  Server nützt deshalb von außen nichts. Gegen `quack_stop` hilft sie nicht.
* **Init- und Post-Start-SQL:** Beide stehen im Secret der Engine (`<pod>-token`, Schlüssel `init.sql` und
  `post-start.sql`), nicht in der Pod-Spec; wer nur Pods lesen darf, sieht sie nicht. Mit Dateizugriff, dem
  DuckDB-Default, kann sie aber jede Session in der Engine aus der Umgebung lesen
  (`read_text('/proc/1/environ')`). Schlüssel im `initSQL` sind also für alle Benutzer der Engine sichtbar;
  besser `PROVIDER credential_chain` mit Workload Identity.
* **Gegenmittel:** ein gehärtetes Profil, siehe [unten](#gehärtete-geteilte-engines).
* **Ressourcen:** `resources.limits` und `settings.memory_limit` je Profil begrenzen eine Engine, `limits.*`
  die Zahl der Engines und Sessions.
* **Laufzeit:** `limits.queryTimeout`, `profiles.<name>.queryTimeout` oder `overrides[].queryTimeout` begrenzen, wie
  lange eine Query laufen darf; danach bricht das Gateway sie ab, sobald ein Request für sie auf die Engine wartet
  ([Design, 5.3](../../docs/design.md#53-query-zeitlimit)). Das Abholen des Ergebnisses zählt mit; `COMMIT` und
  `ROLLBACK` haben kein Limit. Schließt ein Client seine Session, während eine Query läuft, wartet das `DISCONNECT`
  an die Engine, bis das Limit mit ihr fertig ist (die Engine ließe sie sonst weiterlaufen). Eine beantwortete
  Query, deren Ergebnis kein Client abholt, kann auf der Engine weiterrechnen, bis die nächste Query der Session sie
  ersetzt oder die Session endet; eine Grenze, die kein Client umgeht, setzt `SET GLOBAL max_execution_time`
  ([Gehärtete geteilte Engines](#gehärtete-geteilte-engines), „Laufzeit der Engine begrenzen“).
* **Plattenplatz:** Bei Speicherdruck lagert DuckDB Zwischenergebnisse aus: bei `:memory:` nach `/tmp/.tmp` (ein
  `emptyDir` auf der Platte des Nodes), bei einer Datei daneben (`<datei>.tmp`, also auf deren Volume). Ohne
  Grenze darf das bis zu 90 % des freien Platzes belegen; eine einzige Query kann so den Node füllen und andere
  Pods verdrängen. Setzen Sie je Profil `settings.max_temp_directory_size` und darüber
  `resources.limits.ephemeral-storage` (mit passendem `requests`-Wert): Dann scheitert die Query mit einem
  Fehler, statt dass der Node volläuft, und im Notfall räumt das Kubelet nur diese Engine.
* **Sandbox:** Wo vorhanden, isoliert eine Sandbox-Runtime die Engines zusätzlich
  (`profiles.<name>.podTemplate.spec.runtimeClassName`, z. B. gVisor).
* **Namespace:** Pod Security `restricted` ist aktiv. Das Gateway darf alle Secrets des Namespace lesen;
  lassen Sie dort nichts anderes laufen.

### Gehärtete geteilte Engines

Für `GROUP`- und `SERVER`-Engines, deren Benutzer einander nicht in allem vertrauen:

```yaml
profiles:
  shared:
    allowedShareLevels: [GROUP, SERVER]
    initSQL: |
      .read /usr/local/share/quackmux/authorize.sql
    postStartSQL: |
      SET enable_external_access = false;
      SET lock_configuration = true;
```

Die Teile brauchen einander:

1. **`postStartSQL`** läuft direkt nach `quack_serve` auf der Verbindung des Init-SQL. Es kommt wie das Init-SQL
   über das Secret der Engine (`QUACKMUX_POST_START_SQL`, Schlüssel `post-start.sql`). Die beiden Einstellungen
   gehören hierher, weil sie vor `quack_serve` den Start verhindern: Das Token kommt per `getenv()`, und
   `quack_serve` braucht Dateizugriff. Der erste Fehler beendet die Engine. Das Feld braucht das Engine-Image
   derselben quackmux-Version oder ein davon abgeleitetes; ältere Images ignorieren `QUACKMUX_POST_START_SQL`
   ohne Meldung. Ob es lief, zeigt das Log der Engine: `quackmux-engine: ran the post-start SQL`. Ein Gateway
   ohne das Feld lehnt die Konfiguration ab; vor einem Rollback des Gateways muss es also heraus.
2. **`SET enable_external_access = false`** sperrt für alle Sessions Dateien, Netz und `getenv()`; Token und
   Init-SQL in `/proc` sind dann nicht mehr lesbar. Es lässt sich nicht wieder einschalten. Zwei Ausnahmen macht
   DuckDB selbst, wenn die Einstellung zur Laufzeit abgeschaltet wird: Sein Temp-Verzeichnis (`/tmp/.tmp`, bei einer
   Datenbankdatei `<datei>.tmp`) und die Dateien der angehängten Datenbanken (die Datenbankdatei samt WAL)
   bleiben für jede Session les- und schreibbar. Eine Session könnte so die Datenbankdatei zerstören (die Engine
   startet danach nie wieder) oder SQL ins Temp-Verzeichnis schreiben, das `IMPORT DATABASE` ungeprüft ausführt.
   Schreiben (`COPY`, `EXPORT DATABASE`) und `IMPORT DATABASE` sperrt deshalb das Makro. Lesen bleibt möglich:
   die Datenbankdatei, die jede Session ohnehin abfragen kann, und die Auslagerungsdateien im Temp-Verzeichnis,
   darin Zwischenergebnisse und temporäre Tabellen anderer Sessions, sobald DuckDB sie auslagert. Wer das
   ausschließen muss, setzt davor `SET temp_directory = ''`; dann lagert DuckDB nichts aus, und große Queries
   scheitern am `memory_limit`.

   Weitere Ausnahmen erlauben `allowed_paths` (Dateien) und `allowed_directories` (Präfixe von Pfaden und URLs),
   ebenfalls davor gesetzt. Für HTTPS und S3 braucht DuckDB dann auch das CA-Bundle:

   ```sql
   SET allowed_directories = ['/data/import/', 'https://data.example.com/lake/'];
   SET allowed_paths = ['/etc/ssl/certs/ca-certificates.crt'];
   SET enable_external_access = false;
   SET lock_configuration = true;
   ```

   Ohne solche Einträge scheitern alle Tabellenfunktionen und Views über Dateien und URLs, auch Views aus dem
   `initSQL`; Datenbanken, die das `initSQL` per `ATTACH` anhängt, erlaubt DuckDB von selbst. Verzeichnisse in
   `allowed_directories` sollten für den Engine-Benutzer nur lesbar sein (etwa ein read-only Mount). Extensions
   lädt DuckDB nicht mehr, auch nicht per Autoload: Was Sessions brauchen, lädt das `initSQL` (`LOAD spatial;`).
   Im Image sind `quack` und `httpfs` bis dahin schon geladen. `duckdb_extensions()` scheitert.
3. **`SET lock_configuration = true`** sperrt `SET` und `RESET` (außer `schema` und `search_path`) und
   `enable_logging()`. So dreht niemand `quack_authorization_function` oder die Server-Settings zurück. Clients,
   die beim Verbinden selbst `SET`s schicken, scheitern daran; DuckDB-Clients über Quack tun das nicht.
4. **Das Makro `quackmux_authorize`** aus dem Engine-Image
   ([images/engine/authorize.sql](../../images/engine/authorize.sql)) wird `quack_authorization_function`. Der
   Quack-Server ruft es vor jeder Anfrage eines Clients mit ihrem SQL-Text auf, auch vor den Katalog-Abfragen von
   `ATTACH`. Es lehnt Anfragen ab, die eines dieser Wörter enthalten:
   * `quack_*` (Server-Steuerung: `quack_stop`, `quack_serve`, `quack_active_connections`, `quack_identify`, die
     `quack_*`-Settings …) und `quackmux_*` (das Makro selbst);
   * `getenv` und `shell_history` (was die CLI der Engine von stdin gelesen hat);
   * SQL, das nicht im Text der Anfrage steht, umginge jede Textprüfung: `query` vor `(`,
     `json_execute_serialized_sql`, `json_serialize_plan`, `import` mit später `database` und `import_database`
     (`IMPORT DATABASE` führt das SQL von Dateien aus) sowie die externen Ressourcen von DuckDB 2.0
     (`*external_resource*`, `external` mit später `resource`), die Funktionen aufrufen, deren Namen Strings liefern;
   * Dateien und Datenbanken: `copy` mit später `to` oder `from`, `copy_database`, `export` mit später
     `database`, `attach`, `detach` und `connect`;
   * `checkpoint` (`FORCE CHECKPOINT` bricht die Transaktionen aller Sessions ab), `truncate_duckdb_logs` und
     `write_log` (das Log von DuckDB);
   * `create` oder `drop` mit später `secret`, `macro`, `function` oder `trigger` und `add_parquet_key`: Secrets,
     Makros, Trigger und Parquet-Schlüssel gelten für alle Sessions, ein Makro verdeckt sogar eingebaute Funktionen
     und Operatoren gleichen Namens, und ein Trigger führt sein SQL in den Schreibvorgängen anderer Sessions aus;
   * `alter` mit später `database` (`ALTER DATABASE … SET ALIAS` benennt eine Datenbank für alle Sessions um);
   * `schema` mit später `system`.

   Damit lehnt es auch `SELECT connection_id FROM quack_active_connections()` ab, die Anfrage, mit der das Gateway
   nach einem Neustart die Verbindungen der Engine listet, um verwaiste zu schließen
   ([Neustarts und verwaiste Verbindungen](#neustarts-und-verwaiste-verbindungen)). Die Variante
   `quackmux_authorize_listing` lässt genau diesen Text durch, Byte für Byte, ohne etwas davor oder danach; sie
   gehört ins `initSQL` hinter das `.read`:

   ```yaml
       initSQL: |
         .read /usr/local/share/quackmux/authorize.sql
         SET GLOBAL quack_authorization_function = 'quackmux_authorize_listing';
   ```

   Das Makro kann das Gateway nicht von Clients unterscheiden (es sieht nur Verbindungs-ID und Text, und das
   Gateway meldet sich mit dem Engine-Token an wie die Sessions, die es weiterreicht), also darf mit der Variante
   jede Session das Listing abfragen. Die Verbindungs-IDs sind aber Bearer-Credentials: Nach dem Handshake bedient
   der Quack-Server eine Nachricht allein anhand ihrer Verbindungs-ID. Wer Port 9494 der Engine direkt erreicht,
   kann mit einer gelisteten ID die Verbindung einer anderen Session benutzen, am Gateway und seinem Audit-Log
   vorbei: ihre temporären Tabellen und zwischengespeicherten Ergebnisse lesen, SQL in ihrer Transaktion ausführen,
   ihre Query abbrechen oder sie schließen. Die Variante passt deshalb nur, wenn außer dem Gateway niemand den Port
   erreicht: in Kubernetes mit der NetworkPolicy aus `base` und einem CNI, das sie durchsetzt, beim `static`- und
   `process`-Provider nur hinter einer Firewall, die allein das Gateway an den Port lässt.

   Bis das `postStartSQL` beide Einstellungen gesetzt hat, lehnt es jede Anfrage ab: Vorher ist die Engine nicht
   gehärtet, und mit Dateizugriff ließe sich eine Textprüfung umgehen. Das `.read` gehört vor ein `USE` im
   `initSQL`: Der Quack-Server sucht die Funktion auf einer neuen Verbindung in der Default-Datenbank, sonst
   scheitert jede Anfrage („quackmux_authorize does not exist“). Abgelehnte Anfragen scheitern beim Client
   mit `Authorization callback failed: … quackmux_authorize: not allowed on this engine: "quack_stop"` bzw.
   `… the engine is not hardened yet …; try again`.

**Laufzeit der Engine begrenzen:** `SET GLOBAL max_execution_time = <ms>` vor `SET lock_configuration = true` lässt
die Engine jede Anweisung nach dieser Zeit abbrechen („Query exceeded maximum execution time“), auch eine, deren
Ergebnis kein Client abholt und die das Query-Zeitlimit des Gateways deshalb nicht trifft (siehe „Laufzeit“ oben).
Mit `lock_configuration` können Sessions den Wert nicht ändern. Im gehärteten Profil gehört die Zeile ins
`postStartSQL` vor `lock_configuration`, ohne Härtung ins `initSQL`, gefolgt von `SET lock_configuration = true`
(im `initSQL` mit DuckDB 2.0.0 geprüft). Ohne `GLOBAL`, also auch über `settings`, gilt der Wert nur für die
Verbindung des Init-SQL, nicht für die Sessions.

Mit dem `process`-Provider gilt dasselbe Profil, nur mit dem absoluten Pfad von `images/engine/authorize.sql` im
`.read`. Dort laufen die Engines als Benutzer des Gateways: Ohne Härtung liest und schreibt jede Session, was
dieser Benutzer darf (die Konfiguration des Gateways, die Umgebung der anderen Engines samt Token, `~/.duckdbrc`,
das die nächste Engine ausführt), und führt so auch Befehle als dieser Benutzer aus. Ungehärtet taugt der Provider
nur für vertrauenswürdige Clients. Für Engines des `static`-Providers setzen Sie `QUACKMUX_INIT_SQL` und
`QUACKMUX_POST_START_SQL` am Engine-Image selbst ([images/engine/README.md](../../images/engine/README.md),
„Sicherheit“); `initSQL` und `postStartSQL` eines Profils wirken dort nicht, `quackmux validate` warnt.

**Umstellen:** Ein geändertes `initSQL` oder `postStartSQL` ändert den Spec-Hash, aber laufende Engines bleiben
ungehärtet, bis sie enden (`engine.replaceOnSpecChange`): Die alten Engines nicht persistenter Profile nehmen keine
neuen Sessions mehr an, ihre bestehenden laufen höchstens `engine.drainTimeout` weiter. Persistente Profile schicken
neue Sessions weiter auf die alte Engine, bis sie keine mehr hat, spätestens `engine.drainTimeout` lang; startet das
Gateway dabei neu (etwa wegen der geänderten ConfigMap), frühestens nach `engine.recoveryGrace`
([Persistente Daten](#persistente-daten)). Sofort wirkt die Härtung so: in `GET /api/v1/engines` die Engines des
Profils mit `"specCurrent": false` suchen und mit `DELETE /api/v1/engines/{id}` beenden; das schließt ihre Sessions.

**Was das garantiert** (geprüft mit DuckDB 2.0.0 und dem Engine-Image: die 224 Fälle in
[images/engine/authorize-cases.tsv](../../images/engine/authorize-cases.tsv), die der Selbsttest des Images direkt
mit beiden Varianten und Szenario j des Compose-Tests durch das Gateway mit `quackmux_authorize_listing` ausführt):

* Eine Anfrage mit einem der Wörter läuft nicht, wie auch immer es geschrieben ist: in Groß- und Kleinbuchstaben,
  in `"…"` oder `` `…` ``, qualifiziert (`system.main.quack_stop`), mit Kommentaren oder Unicode-Leerzeichen vor
  der Klammer, in einer CTE oder einem `UNION`, in `PREPARE` oder `EXPLAIN`, als zweites Statement, über
  `remote.query(…)`. DuckDB kennt keine andere Schreibweise eines Namens: Bezeichner haben keine
  Escape-Sequenzen, und Namen vergleicht DuckDB nur für ASCII-Buchstaben ohne Groß- und Kleinschreibung.
* Die Wege von DuckDB 2.0.0 und der Extensions des Images, SQL auszuführen, das nicht im Text der Anfrage steht,
  sind gesperrt (`query_table()` erreicht nur Tabellen). Clients können deshalb weder das Makro noch die
  eingebauten Funktionen, die es aufruft, ersetzen oder verdecken, und die Einstellungen nicht zurückdrehen.
* Keine Session schreibt Dateien der Engine: nicht die Datenbankdatei, nicht ihr WAL, nicht das Temp-Verzeichnis.
* Token, Umgebung und der Text von Init- und Post-Start-SQL bleiben unlesbar: kein `getenv()`, kein `/proc`, kein
  `shell_history()`, und `FROM _` (das letzte Ergebnis der CLI der Engine) zeigt nur `started`.
* `ATTACH`, Katalog, Queries, Transaktionen, temporäre Tabellen, Views, Schemas, Uploads, Downloads und `COPY` auf
  der Seite des Clients laufen wie ohne Härtung, ebenso das Stoppen der Engine (`quack_stop`, `FORCE CHECKPOINT`).
* Das Gateway schickt außer Handshake, Heartbeats, `DISCONNECT` und `CANCEL` nur das Listing der Verbindungen.
  `quackmux_authorize` lehnt es ab; `quackmux_authorize_listing` lässt genau diesen Text durch und lehnt jede
  Abwandlung ab (andere Schreibweise oder Spalten, Leerzeichen, ein Kommentar, ein zweites Statement).

Ein eigenes Image (andere DuckDB-Version, weitere Extensions) prüft `docker run --rm --entrypoint
quackmux-engine-selftest <image>` mit denselben Fällen; eine laufende gehärtete Engine oder ein Gateway davor
prüft `QUACK_TOKEN=<token> quackmux-authorize-test <host>:<port>` aus dem Image, mit `QUACKMUX_TEST_LISTING=1`
für eine Engine mit `quackmux_authorize_listing`.

**Was es nicht garantiert:**

* Mit `quackmux_authorize_listing` kann jede Session die Verbindungs-IDs der Engine lesen; wer zusätzlich den
  Port der Engine erreicht, benutzt damit fremde Verbindungen (siehe oben). `quackmux_authorize` gibt die IDs
  nicht heraus.
* Das Makro prüft Text, nicht Bedeutung: Es ist eine Sperrliste. Funktionen und Anweisungen, die eine spätere
  DuckDB-Version oder eine geladene Extension mitbringt, sperrt es nicht. Prüfen Sie die Liste nach jedem Update von
  DuckDB oder den Extensions des Images (`SELECT function_name FROM duckdb_functions()`, Selbsttest).
* Fehlalarme: Auch ein Wort in einem String oder Kommentar führt zur Ablehnung (`SELECT 'attach'`, `-- getenv`,
  `CREATE TABLE notizen AS SELECT 'secret'`, `SELECT 'copy' FROM t`), ebenso ein Wert in einem Filter, den der
  Client an die Engine weiterreicht (`WHERE art = 'connect'`). Makros, Funktionen und Secrets können Clients
  nicht anlegen, Dateien auf der Engine nicht schreiben, auch nicht mit `COPY` auf der Engine.
* Daten trennt es nicht: Jede Session liest und ändert alle Tabellen und angehängten Datenbanken der Engine und
  liest die Auslagerungsdateien (siehe oben). Was das Init-SQL anlegt, sieht jede Session: Pfade angehängter
  Datenbanken (`duckdb_databases()`), Definitionen von Views und Makros (`duckdb_views()`, `duckdb_functions()`),
  Name, Scope und `KEY_ID` von Secrets (`duckdb_secrets()`, Werte geschwärzt), alle Einstellungen
  (`duckdb_settings()`) und den Pfad der Datenbank. Zugangsdaten gehören deshalb weder in `ATTACH`-Strings noch in
  Views, Makros oder `SET`-Anweisungen, sondern in Secrets, am besten mit `PROVIDER credential_chain`. Teure
  Queries bremsen weiter alle.
* Beim Start antwortet `GET /`, und damit die Readiness-Probe, schon während das `postStartSQL` läuft. Sessions,
  die in diesem Moment kommen, lehnt das Makro ab; ohne das Makro liefen ihre Anfragen ungehärtet. Halten Sie das
  `postStartSQL` kurz.
* Jede Anfrage kostet rund 2 ms mehr, weil der Quack-Server das Makro bei jedem `PREPARE` neu übersetzt (gemessen:
  2,1 ms ohne, 4,3 ms mit Makro für eine triviale Query).

## Fehlersuche

```sh
kubectl -n quackmux logs deployment/quackmux -f
kubectl -n quackmux get pods -l app.kubernetes.io/component=engine -L quackmux.io/share-level,quackmux.io/profile
kubectl -n quackmux describe pod quack-user-alice-x7k2p
kubectl -n quackmux logs quack-user-alice-x7k2p
kubectl -n quackmux get events --sort-by=.lastTimestamp
kubectl -n quackmux get pvc -l app.kubernetes.io/component=engine-data -L quackmux.io/share-level,quackmux.io/profile
```

Mit `overlays/ha` zeigt `logs deployment/quackmux` irgendeinen bereiten Pod, nicht unbedingt den Leader; den zeigt
`kubectl -n quackmux logs -l app.kubernetes.io/component=gateway,quackmux.io/leader=true -f`.

| Symptom | Ursache und Abhilfe |
|---|---|
| Engine-Pod bleibt `Pending`; Client: `quackmux: could not provide an engine: engine did not become ready in time (3m0s)` | Zu wenig CPU oder Speicher für die Requests des Profils, ResourceQuota, NodeSelector oder Taints passen nicht. `kubectl describe pod` zeigt den Grund in den Events. Requests verkleinern, Nodes ergänzen oder `startupTimeout` erhöhen (etwa wenn der Cluster-Autoscaler erst einen Node startet). |
| `ImagePullBackOff` / `ErrImagePull` | Image-Name oder Tag falsch, Registry nicht erreichbar, private Registry ohne `imagePullSecrets`. Gateway: Deployment patchen; Engines: `profiles.<name>.image` und `imagePullSecrets`. kind: Images mit `kind load docker-image` laden. Fehlgeschlagene Engine-Pods bleiben `engine.failedRetention` (10 min) zur Analyse stehen. |
| Gateway-Log: `launch failed: … violates PodSecurity "restricted…"` | Ein Pod-Template des Profils enthält einen Container ohne gehärteten Security-Context (z. B. einen Sidecar). Template anpassen oder den Namespace bewusst auf `enforce: baseline` stellen. |
| Gateway-Pod in `CrashLoopBackOff` | `kubectl -n quackmux logs deployment/quackmux --previous` (mit `overlays/ha`: `kubectl -n quackmux logs <pod> --previous`, sonst wählt kubectl einen bereiten Pod): Konfigurationsfehler (unbekanntes Feld), fehlende oder ungültige `users.yaml`, Session-Key kürzer als 16 Bytes. |
| `Authentication failed` | Token unbekannt (in `users.yaml` gehört das sha256 des Tokens ohne `?…`), abgelaufen (`expires`) oder der Benutzer ist `disabled`. Passt der `SCOPE` des Secrets nicht zum Ziel des `ATTACH` (anderer Host oder Port, `localhost` statt `127.0.0.1`), verwendet der Client das Secret nicht. Gateway-Log: `authentication failed` mit Grund; Metrik `quackmux_auth_failures_total`. |
| `quackmux: session closed: idle timeout (6h0m0s). DETACH and ATTACH again to reconnect.` | Die Session hat länger als `session.idleTimeout` keine Query geschickt; Heartbeats zählen nicht. `DETACH` und erneut `ATTACH`, oder `session.idleTimeout` erhöhen (`0s` schaltet ab). Verwandte Gründe: `heartbeat lease expired` (der Client war länger als Lease plus `heartbeatGrace` stumm), `max lifetime`, `engine …` (Engine beendet oder ausgefallen). |
| `INTERRUPT Error: quackmux: the query ran longer than its time limit of 30m0s (profiles.adhoc.queryTimeout) and was cancelled` | Das Query-Zeitlimit ist abgelaufen; die Meldung nennt die Einstellung (`limits.queryTimeout`, `profiles.<name>.queryTimeout` oder `overrides[].queryTimeout`). Die Engine hat den Abbruch bestätigt, die Session bleibt nutzbar. Lief die Query in einer Transaktion (der DuckDB-Client öffnet eine für jede schreibende Anweisung), ist diese abgebrochen, und der Client rollt sie zurück. Das Abholen des Ergebnisses zählt mit, langsame Clients mit großen Ergebnissen brauchen also ein großzügiges Limit. Gateway-Log `query cancelled at its time limit` mit `cancel=cancelled`, Metrik `quackmux_query_timeouts_total{result="cancelled"}`. |
| `INTERRUPT Error: quackmux: the query ran longer than its time limit of 30m0s (profiles.adhoc.queryTimeout); the gateway could not cancel it on the engine, so it may still be running` | Wie oben, aber das CANCEL ist gescheitert (Engine nicht erreichbar, keine frische Lease, oder die Query hatte `query_uuid {0,0}` und nach ihr kam schon ein weiteres PREPARE der Session). Die Query kann auf der Engine weiterlaufen und belegt sie, bis die nächste Query der Session sie ersetzt oder die Session endet. Gateway-Log mit `cancel=failed` und `err`, Metrik `quackmux_query_timeouts_total{result="failed"}`. Eine schreibende Anweisung erst wiederholen, wenn klar ist, ob sie geschrieben hat. |
| `quackmux: the session has 64 queries that are not over, the most the gateway follows; it did not forward this request. Cancel those queries or reconnect.` | Nur mit Query-Zeitlimit: Die Session hat 64 Queries, die die Engine noch ausführen kann, weil der Client viele PREPAREs zugleich offen hat (der DuckDB-Client tut das nicht). Das Gateway leitet dann keine weiteren weiter, statt eine laufende Query zu vergessen und ihr Limit zu verlieren. Queries, deren Abbruch scheiterte, zählen nicht mit. Ein CANCEL des Clients beendet die beantworteten Queries, die übrigen enden mit ihrer Antwort oder ihrem Limit; `DETACH` und `ATTACH` öffnet eine neue Session. Metrik `quackmux_requests_total{outcome="too_many_queries"}`. Der Request lief nicht. |
| `quackmux: session closed: the engine no longer knows this connection. DETACH and ATTACH again to reconnect.` | Nach einem Neustart oder einer Übernahme fand das Gateway die Verbindung des Clients nicht mehr auf seiner Engine: Die Engine hat inzwischen neu gestartet, oder der Client war länger fort als die Engine-Lease seiner Verbindung (`session.backendHeartbeatTimeout`), und die Engine hat sie entfernt ([Neustarts und verwaiste Verbindungen](#neustarts-und-verwaiste-verbindungen)). `DETACH` und erneut `ATTACH`. |
| Gateway-Log: `cannot list the connections of a recovered engine: its orphaned connections stay open` | Das Gateway kann die verwaisten Verbindungen dieser Engine nicht schließen; sie bleiben bis zum Ende der Engine. Mit `hint` zu 2048: Die Engine hat mehr als 2048 Verbindungen. Sonst war die Engine nicht erreichbar. |
| Gateway-Log: `not closing orphaned connections: the authorization function of the engine refuses the listing` | Die gehärtete Engine läuft mit `quackmux_authorize` (oder einem älteren oder eigenen Makro), das das Listing ablehnt; ihre verwaisten Verbindungen bleiben bis zum Ende der Engine. Erreicht außer dem Gateway niemand ihren Port, lässt `quackmux_authorize_listing` das Listing durch ([Gehärtete geteilte Engines](#gehärtete-geteilte-engines)). |
| `Failed to send message` oder SSL-Fehler beim `ATTACH` | HTTPS und HTTP passen nicht zusammen. Ohne `server.tls` spricht das Gateway Plain HTTP; für jeden Host außer `localhost` braucht der Client dann `(DISABLE_SSL true)`. Umgekehrt darf `DISABLE_SSL` nicht gesetzt sein, wenn Gateway oder Load Balancer TLS sprechen (das Gateway antwortet sonst mit HTTP 400). Steht ein L7-Proxy davor, sind auch dessen Timeouts und Body-Limits verdächtig (siehe „Verbinden“). |
| `quackmux: gateway is not ready: not the leader` | Der Request landet bei einem Follower: ein Port-Forward auf einen bestimmten Pod oder, mit `overlays/ha`, ein Service ohne `quackmux.io/leader: "true"` im Selektor. `svc/quackmux` verwenden bzw. den Selektor ergänzen; `quackmux_requests_total{outcome="not_leader"}` zählt solche Requests. Der Request lief nicht. |
| HA: Gateway-Pod bleibt `0/1 READY`, `/readyz` meldet `label quackmux.io/leader of pod … not yet removed: …` | Der Pod kann sein Leader-Label nicht entfernen, meist weil die API nicht erreichbar ist; bis dahin bekommt er keinen Traffic. `waiting to campaign for leadership` ist in den ersten `3 × ha.retryPeriod` nach dem Start normal, `stepping down: the lease is no longer fresh` bei einem Leader, der seine Lease verloren hat, bis zu seinem Neustart als Follower. |
| HA: Follower bleibt `0/1 READY`, `/readyz` meldet `no request for lease … has succeeded …`, Log: `Error retrieving lease lock … forbidden` | Der Follower darf die Lease nicht nutzen oder erreicht die API nicht und könnte nie übernehmen: Im Namespace der Lease (`ha.namespace`, sonst der Engine-Namespace) fehlen Role und RoleBinding aus `base/rbac.yaml`. |
| HA: Leader bleibt `0/1 READY` mit `starting up`, Service ohne Endpunkt, Log: `not allowed to list/watch engine pods …` | Der Leader kann seine Engine-Registry nicht synchronisieren, und solange er die Lease verlängert, übernimmt niemand. Rechte im Engine-Namespace ergänzen (`base/rbac.yaml`); danach bedient er ohne Neustart. |
| HA: Gateway startet nicht, Log: `ha: leader election needs the gateway to run as a Kubernetes pod …`, `ha: the gateway pod … does not exist`, `ha: the gateway may not label its pod …` oder `ha: this replica could never lead …` | `POD_NAME` und `POD_NAMESPACE` fehlen oder nennen nicht diesen Pod (Downward-API wie in `base/deployment.yaml`), der Role fehlt `patch` auf Pods im Namespace des Gateways, oder im Namespace der Lease oder der Engines fehlen Role und RoleBinding (`base/rbac.yaml`; die Meldung nennt die fehlenden Rechte). |
| `quackmux: gateway is not ready: stepping down: the lease is no longer fresh` | Die Verbindung des Clients führte noch zu einem Leader, der seine Lease verloren hat (etwa nach einem Einfrieren); er startet als Follower neu. Der Request lief nicht und kann wiederholt werden. |
| `quackmux: could not provide an engine: engine limit reached …` | `limits.maxEngines`, `maxEnginesPerUser` oder `maxStartingEngines` erreicht; `GET /api/v1/engines` zeigt, wer sie belegt. |
| `quackmux: not permitted: …` | Share-Level, Profil, Subdomain oder Gruppe sind für den Benutzer nicht erlaubt (`allowedShareLevels`, `allowedUsers`, `allowedGroups`, `allowedSubdomains`, `overrides`). |
| `quackmux: could not provide an engine: … the previous engine of this key still holds its data volume …` | Persistentes Profil: Die vorige Engine des Schlüssels läuft noch aus (Drain) oder ihr Pod hängt in `Terminating` (Node ausgefallen, siehe [Persistente Daten](#persistente-daten)); `GET /api/v1/engines` zeigt sie. Mit `(engine pod … exists …)` gibt es einen Pod des Schlüssels, den das Gateway nicht kennt: `kubectl get pods -l quackmux.io/key-hash=<hash>`. Ist sein Token-Secret lesbar, übernimmt das Gateway ihn nach wenigen Sekunden. |
| `quackmux: could not provide an engine: … this gateway does not hold a fresh leader lease` | Persistentes Profil mit HA: Das `ATTACH` erreichte einen Leader, der gerade seine Lease verliert. Wiederholen. |
| `launch failed: … refusing to mount a claim that does not belong to the engine key` | Unter dem Namen liegt ein fremder Claim (andere Labels oder anderes `quackmux.io/key`). Das Gateway überschreibt ihn nie; prüfen und gegebenenfalls löschen. |
| `launch failed: data volume claim … is being deleted` | Der Claim wird gerade gelöscht; ist er weg, legt das nächste `ATTACH` ein neues, leeres Volume an. |
| `launch failed: create data volume claim …: … exceeded quota` | ResourceQuota des Namespace erschöpft (`count/persistentvolumeclaims`, `requests.storage`). |
| Engine-Pod bleibt `Pending`, sein Claim ebenfalls | Provisioner oder StorageClass (`kubectl describe pvc <name>`); ohne Default-StorageClass `persistence.storageClassName` setzen. Die Zeit zählt gegen `startupTimeout`. Ein bestehender Claim behält StorageClass und Access-Mode: nach der Korrektur des Profils Engine beenden und den (nie gebundenen, also leeren) Claim löschen, `DELETE /api/v1/volumes/{name}`. |
| `DELETE /api/v1/volumes/…` antwortet 409 `… pod … mounts it` oder `… engine pod … of its key exists` | Ein Pod nutzt den Claim, auch einer, den das Gateway nicht kennt (Debug- oder Backup-Pod, eine Engine eines anderen Gateway-Prozesses). Den Pod beenden, dann erneut. |
| `GET` oder `DELETE /api/v1/volumes…` antwortet 503 | Diese Replika ist nicht der Leader oder kennt ihre Engines noch nicht. Port-Forward auf den Leader (`holderIdentity` der Lease) oder kurz warten. |
| Engine-Pod bleibt `Pending`: `… PersistentVolumeClaim with ReadWriteOncePod access mode already in-use by another pod` | Ein anderer Pod nutzt den Claim, etwa ein Debug-Pod: `kubectl get pods -l quackmux.io/key-hash=<hash>` und `kubectl describe pvc <name>`. |
| Engine scheitert mit `Could not set lock on file "/data/engine.duckdb"` | Ein zweiter Prozess hält die Datei (nur mit `ReadWriteOnce`, einem fremden Pod oder einem PVC im `podTemplate` möglich). |
| `Authorization callback failed: … quackmux_authorize: not allowed on this engine: "…"` | Das Makro einer gehärteten Engine lehnt die Anfrage ab; in Anführungszeichen steht, woran ([Gehärtete geteilte Engines](#gehärtete-geteilte-engines)). Ein Fehlalarm (das Wort steht nur in einem String, Kommentar oder Filterwert) lässt sich umformulieren. |
| `… quackmux_authorize: the engine is not hardened yet …; try again` | Die Engine führt ihr `postStartSQL` noch aus, oder es setzt nicht beide Einstellungen. Fehlt im Log der Engine `ran the post-start SQL`, kennt ihr Image `QUACKMUX_POST_START_SQL` nicht (älteres oder eigenes Image). |
| `… quackmux_authorize does not exist` | Das `.read` des Makros steht im `initSQL` nach einem `USE`; es gehört davor. |
| Gehärtete Engine: `Permission Error: Cannot access file "…" - file system operations are disabled by configuration` | Pfad oder URL fehlt in `allowed_paths` bzw. `allowed_directories`, gesetzt im `postStartSQL` vor `enable_external_access = false`; auch Views aus dem `initSQL` brauchen den Eintrag. |
| Gehärtete Engine: `Permission Error: Cannot read a CA certificate bundle: …` | HTTPS oder S3: `SET allowed_paths = ['/etc/ssl/certs/ca-certificates.crt'];` ins `postStartSQL` vor `enable_external_access = false`. |
| Gehärtete Engine: `Loading external extensions is disabled through configuration` oder `Extension Autoloading Error: …` | Extensions im `initSQL` laden (`LOAD spatial;`); nach dem `postStartSQL` lädt DuckDB keine mehr. |
