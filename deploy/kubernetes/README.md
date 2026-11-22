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
| `quackmux_requests_total{type,outcome}`, `quackmux_request_seconds{type}` | Quack-Requests nach Nachrichtentyp |
| `quackmux_bytes_total{direction}` | durchgereichte Bytes (`in`: Client→Engine, `out`: Engine→Client) |
| `quackmux_connect_failures_total{reason}`, `quackmux_auth_failures_total{method}` | gescheiterte Handshakes, fehlgeschlagene Authentifizierungen |
| `quackmux_requests_in_flight`, `quackmux_is_leader` | laufende Requests; 1 auf der Replika, die bedient |
| `quackmux_leader_label`, `quackmux_leader_label_updates_total{action,result}` | nur mit HA: 1, solange der eigene Pod das Label `quackmux.io/leader` trägt (von der API bestätigt); Patches des Labels (`set`, `remove`, `remove_stale`) nach Ergebnis. Alarm (über den `PodMonitor`), wenn `max(quackmux_is_leader)` 1 ist, aber `max(quackmux_leader_label)` länger als eine Minute 0: Der Leader bekommt keinen Traffic. Quack-Requests, die bei einem Follower ankommen, zählt `quackmux_requests_total` mit `outcome="not_leader"`. |

Das Audit-Log (`audit.enabled`, in der Base an) schreibt jede Query als JSON-Zeile mit Benutzer, Session,
Engine und gekürztem SQL ins Gateway-Log. Statements, die Zugangsdaten enthalten können, stehen dort ohne ihren
Text (`"sql":"[redacted: …]"`, `"redacted":true`): `CREATE SECRET` und alles, was `secret`, `passw`, `pwd`,
`token`, `AccountKey` oder eine SAS-Signatur (`sig=`) enthält, auch in Namen wie `s3_secret_access_key`, sowie URLs
mit Benutzer und Passwort. Das ist eine Heuristik: Behandeln Sie das Audit-Log trotzdem als vertraulich.

```sh
kubectl -n quackmux logs deployment/quackmux | grep '"component":"audit"'
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
* Ein Follower meldet sich ready, sobald er sich um die Lease bewirbt (`3 × ha.retryPeriod`, also 6 s nach dem
  Start) und die API bestätigt hat, dass sein Pod das Label nicht trägt. Ein Label, das ein früherer Container
  desselben Pods hinterlassen hat, entfernt er dabei. Bis dahin nennt `/readyz` den Grund.
* **Voraussetzungen:** Das Gateway läuft als Pod und kennt dessen Namen und Namespace aus der Downward-API
  (`POD_NAME`, `POD_NAMESPACE`, wie in `base/deployment.yaml`), und seine Role erlaubt `patch` auf Pods im eigenen
  Namespace (`base/rbac.yaml`). Fehlt etwas davon, startet das Gateway mit `ha.enabled` nicht und nennt den Grund.
  Leader-Election außerhalb des Clusters (per Kubeconfig) ist deshalb nicht möglich.
* Leader anzeigen: `kubectl -n quackmux get pods -L quackmux.io/leader` oder
  `kubectl -n quackmux get lease quackmux-default -o jsonpath='{.spec.holderIdentity}'` (Metriken
  `quackmux_is_leader`, `quackmux_leader_label`).
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
  deshalb noch `ha.deregistrationDelay` (3 s) nach dem Entfernen des Labels mit einem Fehler; ein beendeter Pod
  ließe sie bis zu zwei Minuten im Verbindungsaufbau hängen. Queries, die beim Herunterfahren noch laufen, bekommen
  kurz vor `server.shutdownTimeout` einen Fehler und werden auf der Engine abgebrochen, damit libcurl sie nicht
  unbemerkt ein zweites Mal sendet.
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
* Geprüft mit [test/e2e/kind/ha.sh](../../test/e2e/kind/ha.sh): Übergabe, eingefrorener Leader, Rollout und
  PodDisruptionBudget mit angehängten DuckDB-2.0-Clients.
* Eine bevorzugte `podAntiAffinity` verteilt die Replikas auf verschiedene Nodes.
* `overlays/ha/config.yaml` ersetzt `base/config.yaml` vollständig. Halten Sie beide Dateien bis auf den
  Abschnitt `ha` gleich.

**Rollout (neues Image, neue Konfiguration):** `kubectl apply -k …` genügt, `kubectl rollout status` meldet das
Ende (in kind nach 13–17 s). Mit `maxSurge: 1` und `maxUnavailable: 0` startet Kubernetes zuerst einen neuen Pod
und wartet, bis er als Follower ready ist. Dann entfernt es den alten Follower: Der Leader gibt seinem Pod höhere
Löschkosten (`controller.kubernetes.io/pod-deletion-cost`; ein Wert aus dem Pod-Template wird überschrieben),
deshalb geht er nicht zuerst. Ist auch der zweite neue Pod ready, endet der alte Leader und übergibt an einen der
neuen Pods. Die Lease wechselt je Rollout genau einmal; es sind immer mindestens zwei Replikas ready, kurzzeitig
laufen drei Pods. Fehlt im Cluster der Platz für einen dritten Gateway-Pod, setzen Sie im eigenen Overlay
`maxSurge: 0` und `maxUnavailable: 1`: Dann geht zuerst der Follower, und bis sein Ersatz ready ist, gibt es keinen
Standby. `kubectl rollout status` meldet das Ende, sobald die neuen Pods bereit sind; der alte Leader lässt seine
laufenden Requests danach noch auslaufen (höchstens `server.shutdownTimeout`).

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
* Vorher prüfen: `quackmux validate -config meine-umgebung/config.yaml`. Unbekannte Felder sind Fehler. Ohne
  Cluster prüft `validate` auch, was das Gateway beim Start ablehnen würde: Provider-Einstellungen, jedes Profil
  einmal als Pod gerendert, Lease-Name und Timings der Leader-Election. Token- und Secret-Dateien liest es nicht.
* `kubectl apply -k …` erzeugt eine neue ConfigMap (neuer Hash) und rollt das Gateway neu aus (Base: kurze
  Unterbrechung, Sessions bleiben; HA: siehe oben). Die alten ConfigMaps und Secrets mit Hash-Suffix bleiben
  liegen; `kubectl rollout undo` braucht sie. Löschen Sie sie erst, wenn Sie nicht mehr zurückrollen wollen.
* Geänderte Profile: Engines mit der alten Spec gehen auf DRAINING (`engine.replaceOnSpecChange`). Neue
  Sessions bekommen neue Engines, bestehende laufen auf der alten weiter, höchstens `engine.drainTimeout`.
* Anderer Namespace: `namespace` im eigenen Overlay setzen (Kustomize benennt den Namespace um und passt die
  RoleBinding an) und `provider.kubernetes.namespace` in der Konfiguration ändern oder weglassen; ohne Angabe
  starten die Engines im Namespace des Gateways.

## Persistente Daten

Engines halten ihren Zustand im Speicher (`database: ":memory:"`) und verlieren ihn mit dem Pod. Ein PVC im
`podTemplate` ist **kein** Speicher pro Benutzer: Das Gateway rendert das Template für jede Engine des Profils
gleich. Alle Engines des Profils mounten also dasselbe Volume: jeder Benutzer, jede Gruppe und Subdomain, jede
Pool-Engine und die Ersatz-Engine, die neben einer DRAINING-Engine anläuft. Mit `database: /data/engine.duckdb`
öffnen sie dieselbe Datei. Eine DuckDB-Datei kann nur ein Prozess schreibend öffnen; jede weitere Engine scheitert
beim Start (`Could not set lock on file`). Auf Netzwerk-Dateisystemen ohne verlässliche Locks droht Datenverlust.

* **Gemeinsame Daten read-only anhängen:** PVC (`ReadOnlyMany`) im Template und im `initSQL`
  `ATTACH '/data/lake.duckdb' AS lake (READ_ONLY)`. Das verträgt beliebig viele Engines.
* **Schreibbare Daten in einen externen Katalog:** etwa DuckLake mit Postgres-Katalog und Objektspeicher (Beispiel
  in [images/engine/README.md](../../images/engine/README.md)).
* **Eine schreibbare Datei auf einem PVC** trägt nur ein Profil, das nie mehr als eine Engine hat:
  `allowedShareLevels: [SERVER]`, `pool.size: 1`, Benutzer, die nur eine Subdomain nutzen (`allowedUsers`),
  `maxLifetime` 0 und `engine.replaceOnSpecChange: false`, sonst läuft eine Ersatz-Engine neben der alten an. Das
  Gateway erzwingt das nicht.

PVCs pro Engine-Schlüssel (etwa pro Benutzer) legt das Gateway noch nicht an.

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
* **Init-SQL:** Es steht im Secret der Engine (`<pod>-token`, Schlüssel `init.sql`), nicht in der Pod-Spec; wer
  nur Pods lesen darf, sieht es nicht. In der Engine selbst kann es aber jede Session aus der Umgebung lesen
  (`read_text('/proc/1/environ')`). Schlüssel im `initSQL` sind also für alle Benutzer der Engine sichtbar;
  besser `PROVIDER credential_chain` mit Workload Identity.
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
| `quackmux: gateway is not ready: not the leader` | Der Request landet bei einem Follower: ein Port-Forward auf einen bestimmten Pod oder, mit `overlays/ha`, ein Service ohne `quackmux.io/leader: "true"` im Selektor. `svc/quackmux` verwenden bzw. den Selektor ergänzen; `quackmux_requests_total{outcome="not_leader"}` zählt solche Requests. Der Request lief nicht. |
| HA: Gateway-Pod bleibt `0/1 READY`, `/readyz` meldet `label quackmux.io/leader of pod … not yet removed: …` | Der Pod kann sein Leader-Label nicht entfernen, meist weil die API nicht erreichbar ist; bis dahin bekommt er keinen Traffic. `waiting to campaign for leadership` ist in den ersten `3 × ha.retryPeriod` nach dem Start normal, `stepping down: the lease is no longer fresh` bei einem Leader, der seine Lease verloren hat, bis zu seinem Neustart als Follower. |
| HA: Gateway startet nicht, Log: `ha: leader election needs the gateway to run as a Kubernetes pod …`, `ha: the gateway pod … does not exist` oder `ha: the gateway may not label its pod …` | `POD_NAME` und `POD_NAMESPACE` fehlen oder nennen nicht diesen Pod (Downward-API wie in `base/deployment.yaml`), oder der Role fehlt `patch` auf Pods im Namespace des Gateways (`base/rbac.yaml`). |
| `quackmux: could not provide an engine: engine limit reached …` | `limits.maxEngines`, `maxEnginesPerUser` oder `maxStartingEngines` erreicht; `GET /api/v1/engines` zeigt, wer sie belegt. |
| `quackmux: not permitted: …` | Share-Level, Profil oder Gruppe sind für den Benutzer nicht erlaubt (`allowedShareLevels`, `allowedUsers`, `allowedGroups`, `overrides`). |
