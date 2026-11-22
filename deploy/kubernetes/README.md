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
```

* Zwei Replikas wählen über die Lease `quackmux-default` einen Leader. **Nur der Leader ist ready** und bekommt
  über den Service Traffic. Der Follower zeigt `0/1 READY` und Readiness-Fehler (`not the leader`); das ist
  gewollt. Deshalb meldet `kubectl rollout status` (wie auch der Health-Check von Argo CD) für dieses Overlay
  nie Erfolg: Es wartet auf „1 of 2 updated replicas are available“. Den Zustand zeigt
  `kubectl -n quackmux get deployment quackmux`: fertig bei `UP-TO-DATE 2`, `AVAILABLE 1`; die Bedingung
  `Available` ist dann erfüllt (`kubectl -n quackmux wait --for=condition=Available deployment/quackmux`).
* Leader anzeigen: `kubectl -n quackmux get lease quackmux-default -o jsonpath='{.spec.holderIdentity}'`
  (oder die Metrik `quackmux_is_leader`).
* **Geordnetes Herunterfahren** (Pod löschen, Rollout, Drain): Der Leader meldet sich sofort nicht mehr ready, gibt
  die Lease frei und lässt danach seine laufenden Requests auslaufen. Der wartende Follower übernimmt innerhalb
  von Sekunden (in kind gemessen: 1–4 s); ein frisch gestarteter Ersatz-Pod bewirbt sich erst nach
  `3 × ha.retryPeriod` (6 s). Queries, die beim
  Herunterfahren noch laufen, bekommen kurz vor `server.shutdownTimeout` einen Fehler und werden auf der Engine
  abgebrochen, damit libcurl sie nicht unbemerkt ein zweites Mal sendet.
* **Ausfall des Leaders** (Node weg, Prozess hängt): Der Follower übernimmt, wenn die Lease abgelaufen ist
  (`ha.leaseDuration`, 15 s, plus Retry und Readiness-Probe; in kind gemessen: etwa 20 s). Kann ein Leader die
  Lease nicht verlängern, beendet sich sein Prozess und startet als Follower neu. Wacht ein eingefrorener Leader
  wieder auf, tritt er zurück, ohne Sessions zu schließen oder die Lease des neuen Leaders anzutasten. Clients,
  deren Verbindung noch zum eingefrorenen Pod führt, warten, bis er aufwacht oder neu gestartet wird.
* In beiden Fällen findet der neue Leader die Engines wieder und übernimmt die Sessions mit deren nächstem
  Heartbeat oder Request. Die Sessions überleben. In der kurzen Lücke ohne bereiten Endpunkt (ein bis drei
  Sekunden, bis der Service den neuen Leader kennt) scheitern Requests mit „Could not connect to server“; sie
  laufen nicht und können wiederholt werden.
* Geprüft mit [test/e2e/kind/ha.sh](../../test/e2e/kind/ha.sh): Übergabe, eingefrorener Leader, Rollout und
  PodDisruptionBudget mit angehängten DuckDB-2.0-Clients.
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

Die alte ReplicaSet ersetzt den gelöschten Leader sofort durch einen Pod mit dem alten Template. Dieser Pod
bewirbt sich erst nach `3 × ha.retryPeriod` (6 s) um die Lease, wie jeder frisch gestartete Prozess. Bis dahin
hat der neue Pod sie längst übernommen, und Kubernetes räumt den alten Pod ab.

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
* Geänderte Profile: Engines mit der alten Spec gehen auf DRAINING (`engine.replaceOnSpecChange`). Neue
  Sessions bekommen neue Engines, bestehende laufen auf der alten weiter, höchstens `engine.drainTimeout`.
  Persistente Profile: Die alte Engine nimmt weiter Sessions an, bis sie keine mehr hat (spätestens
  `engine.drainTimeout`); erst dann startet die neue ([Persistente Daten](#persistente-daten)).
* Anderer Namespace: `namespace` im eigenen Overlay setzen (Kustomize benennt den Namespace um und passt die
  RoleBinding an) und `provider.kubernetes.namespace` in der Konfiguration ändern oder weglassen; ohne Angabe
  starten die Engines im Namespace des Gateways. Die Daten-Volumes persistenter Profile liegen im Namespace der
  Engines: Im neuen bekommt jeder Schlüssel ein neues, leeres Volume, die alten bleiben im alten Namespace
  ([Persistente Daten](#persistente-daten)).

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
  Moment, in dem der Request die Kubernetes-API erreicht: Das clientseitige Rate-Limit (`provider.kubernetes.qps`
  und `burst`) kann Requests bei vielen gleichzeitigen Starts sekundenlang zurückhalten. Die Leader-Election hat
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
4. Den Hilfs-Pod löschen; solange er den Claim mountet, startet keine Engine des Schlüssels. Dann attachen.

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
kubectl -n quackmux get pvc -l app.kubernetes.io/component=engine-data -L quackmux.io/share-level,quackmux.io/profile
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
