# Helm-Chart für quackmux

Das Chart installiert das quackmux-Gateway mit denselben Objekten wie die Kustomize-Manifeste in
[deploy/kubernetes](../../kubernetes): mit den Default-Werten die von `base`, mit `ha.enabled` die von
`overlays/ha`. Das Gateway startet die Engines (DuckDB 2.0 mit `quack_serve`) selbst als Pods im Namespace des
Releases und räumt sie nach Timeouts wieder ab. Betrieb, Konfiguration, Persistenz, Härtung und Fehlersuche
beschreibt [deploy/kubernetes/README.md](../../kubernetes/README.md); hier steht, was beim Chart anders ist.

## Voraussetzungen

* Kubernetes 1.30 oder neuer, Helm 3 oder 4 (CI prüft 3.22 und 4.3). Helm 3 braucht für `helm template` die
  Angabe `--kube-version 1.30.0` (oder neuer), weil es sonst Kubernetes 1.20 annimmt.
* Ein CNI, das NetworkPolicies durchsetzt, und die Images `ghcr.io/lldeters/quackmux` und
  `ghcr.io/lldeters/duckdb-engine` wie bei Kustomize ([Voraussetzungen](../../kubernetes/README.md#voraussetzungen)).
  Die Chart-Version ist das quackmux-Release; das Gateway-Image hat dessen Tag, das Engine-Image steht in der
  Konfiguration (`profiles.<name>.image`).
* Ein eigener Namespace pro Release: Die Role des Gateways darf alle Secrets darin lesen, und die NetworkPolicies
  gelten für alle Engines des Namespaces.

## Installation

1. Namespace mit Pod Security `restricted` anlegen (Kustomize legt ihn selbst an, ein Helm-Release nicht):

   ```sh
   kubectl create namespace quackmux
   kubectl label namespace quackmux pod-security.kubernetes.io/enforce=restricted \
     pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted
   ```

2. Benutzer anlegen: `quackmux token` liefert ein Token für den Benutzer und dessen sha256, das in eine
   Values-Datei kommt (Format: [examples/users.yaml](../../../examples/users.yaml)):

   ```yaml
   # meine-werte.yaml
   users:
     - name: alice
       groups: [analytics]            # die erste Gruppe ist der Default für share_level=group
       tokens:
         - sha256: "<sha256 aus quackmux token>"
           description: "Laptop"
   ```

3. Installieren und warten:

   ```sh
   helm install quackmux deploy/helm/quackmux -n quackmux -f meine-werte.yaml
   kubectl -n quackmux rollout status deployment/quackmux
   ```

   Ein Release veröffentlicht das Chart auch als Paket (`quackmux-X.Y.Z.tgz` am GitHub-Release,
   [docs/releasing.md](../../../docs/releasing.md)); `helm install` nimmt statt des Verzeichnisses auch dessen
   URL.

Session-Key und Admin-Token erzeugt das Chart selbst ([Secrets](#secrets)). Den Admin-Token liest man so:

```sh
kubectl -n quackmux get secret quackmux-admin-token -o jsonpath='{.data.token}' | base64 -d
```

Mit dem Release-Namen `quackmux` heißen die Objekte wie bei Kustomize: Deployment und Service `quackmux`,
ServiceAccount, Role und RoleBinding `quackmux-gateway`, ConfigMap `quackmux-config`, Secrets
`quackmux-session-key`, `quackmux-admin-token`, `quackmux-users`. Ein anderer Release-Name ohne „quackmux“
ergibt `<release>-quackmux`; `fullnameOverride` setzt den Namen direkt. Verbinden, Admin-API und Metriken wie in
[deploy/kubernetes/README.md](../../kubernetes/README.md#verbinden), mit dem Service-Namen des Releases.

## Werte

Die vollständige Liste mit Kommentaren steht in [values.yaml](values.yaml). Unbekannte Schlüssel sind Fehler
([values.schema.json](values.schema.json)): Ein Tippfehler wie `ha.enable` fällt nicht still auf einen Default
zurück.

| Wert | Default | Bedeutung |
|---|---|---|
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/lldeters/quackmux`, `appVersion` des Charts, `IfNotPresent` | Gateway-Image |
| `imagePullSecrets` | `[]` | Pull-Secrets für das Gateway-Image; Engines: `profiles.<name>.imagePullSecrets` |
| `replicas` | 1, mit `ha.enabled` 2 | mehr als eine Replika nur mit `ha.enabled` |
| `ha.enabled` | `false` | Leader-Election wie `overlays/ha` ([Hochverfügbarkeit](#hochverfügbarkeit)) |
| `ha.rollingUpdate.maxSurge`, `…maxUnavailable` | 1, 0 | Rollout mit `ha.enabled` |
| `configYaml` | leer: `base/config.yaml` bzw. `overlays/ha/config.yaml` | die vollständige Gateway-Konfiguration als Text |
| `config` | `{}` | Änderungen an der Konfiguration ([Konfiguration](#konfiguration)) |
| `users` | `[]` | Benutzer für `auth.mode: static`; leer: niemand kann sich anmelden. Was das Gateway ablehnen würde (unbekannte Felder wie `disable`, doppelte Benutzer oder Tokens, ein `sha256` ohne 64 Hex-Ziffern), lehnt schon das Chart ab |
| `secrets.sessionKey`, `secrets.adminToken`, `secrets.users` | erzeugt bzw. aus `users` | `existingSecret`: ein vorhandenes Secret |
| `goMemLimit` | `1800MiB` | `GOMEMLIMIT`, etwa 10 % unter `resources.limits.memory` |
| `resources` | Requests 250m/512 MiB, Limits 2 CPU/2 GiB | wie `base` |
| `readinessProbe`, `livenessProbe` | wie `base` | |
| `podSecurityContext`, `securityContext` | wie `base` (Pod Security `restricted`) | |
| `terminationGracePeriodSeconds` | 45 | `server.shutdownTimeout` (30 s) plus Reserve; beide zusammen ändern |
| `podAnnotations`, `podLabels` | `prometheus.io/*`, keine | am Pod-Template |
| `extraEnv`, `extraVolumes`, `extraVolumeMounts` | keine | etwa ein TLS-Zertifikat für `server.tls` (Beispiel in `values.yaml`) |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName` | keine; mit `ha.enabled` eine bevorzugte `podAntiAffinity` | Scheduling des Gateways (nicht der Engines: die stehen im Profil) |
| `serviceAccount.create`, `.name`, `.annotations`, `rbac.create` | an, `<name>-gateway` | ServiceAccount, Role und RoleBinding im Namespace des Releases |
| `service.type`, `service.annotations` | `ClusterIP` | Service mit Quack-Port 9494 und Admin-Port 9495 |
| `externalService.*` | aus | zweiter Service nur mit dem Quack-Port, Typ `LoadBalancer` (oder `NodePort`, dann ohne `loadBalancerSourceRanges` und `loadBalancerClass`), `externalTrafficPolicy: Local` |
| `networkPolicy.enabled`, `.adminFrom`, `.engineEgress` | an, keine, wie `base` | NetworkPolicies; `adminFrom` öffnet den Admin-Port etwa für Prometheus aus einem anderen Namespace |
| `fullnameOverride` | leer | Name der Objekte |

Von außerhalb des Clusters erreichen Clients das Gateway über `externalService` (L4-Load-Balancer, nur Port
9494; mit `ha.enabled` wählt er wie der Service den Leader). Kein L7-Ingress davor
([Von außen](../../kubernetes/README.md#von-außen-l4-load-balancer)).

## Konfiguration

Ohne Angaben enthält die ConfigMap die Konfiguration von `base` (mit `ha.enabled` die von `overlays/ha`), samt
Kommentaren. Das Chart führt dafür Kopien beider Dateien unter [files/](files); CI scheitert, sobald sie von den
Originalen abweichen. Zwei Wege, sie zu ändern:

* **`config`: Änderungen hineinmischen.** Maps werden zusammengeführt, jeder andere Wert ersetzt den bisherigen,
  `null` entfernt einen Schlüssel. Die ConfigMap enthält dann die zusammengeführte Konfiguration ohne die
  Kommentare der Datei.

  ```yaml
  config:
    limits:
      maxEngines: 80
    profiles:
      default:
        image: registry.example.com/duckdb-engine:2.0.0-1.2.3   # eigene Registry
        imagePullSecrets: [{name: registry}]
      large: null                                             # Profil entfernen
  ```

* **`configYaml`: eine eigene, vollständige Datei** (wie `configMapGenerator` mit `behavior: replace`), auf die
  `config` dann noch angewendet wird:

  ```sh
  helm upgrade quackmux deploy/helm/quackmux -n quackmux -f meine-werte.yaml --set-file configYaml=config.yaml
  ```

Vorher prüfen: `helm template` zeigt die ConfigMap, `quackmux validate -config config.yaml` prüft eine eigene Datei
wie das Gateway beim Start.

```sh
helm template quackmux deploy/helm/quackmux -n quackmux -f meine-werte.yaml --kube-version 1.30.0 \
  --show-only templates/configmap.yaml
```

* Eine geänderte Konfiguration rollt das Gateway aus (Annotation `checksum/config` am Pod-Template). Ohne HA ist
  das eine kurze Unterbrechung, die Sessions überleben; mit HA übergibt der Rollout an einen neuen Pod.
* **Die Engines laufen im Namespace des Releases.** In einem anderen Namespace als `quackmux` setzt das Chart
  `provider.kubernetes.namespace` der mitgelieferten Konfiguration auf den Namespace des Releases. Eine eigene
  Konfiguration lässt den Wert weg oder nennt diesen Namespace; ein anderer bricht `helm install` mit einem Fehler
  ab: Role, NetworkPolicies und die Daten-Volumes persistenter Profile gehören in den Namespace der Engines.
* `ha.enabled` der Konfiguration muss zum Wert `ha.enabled` des Charts passen, sonst bricht das Chart ab.
* `server.sessionKeyFile`, `admin.tokenFile` und `auth.static.file` zeigen auf die Pfade, unter denen das Chart
  die Secrets einhängt (`/etc/quackmux/session/key`, `/etc/quackmux/admin/token`,
  `/etc/quackmux/users/users.yaml`); eine eigene Konfiguration übernimmt sie.

## Secrets

| Secret | Schlüssel | ohne `existingSecret` |
|---|---|---|
| `<name>-session-key` | `key` | 43 zufällige Zeichen, beim ersten `helm install` erzeugt |
| `<name>-admin-token` | `token` | ebenso |
| `<name>-users` | `users.yaml` | aus dem Wert `users` |

* `helm upgrade` behält Session-Key und Admin-Token: Das Chart liest die Werte der Secrets im Cluster (`lookup`)
  und schreibt sie unverändert zurück. Sessions überleben Upgrades deshalb wie Rollouts.
* Öffentliche Platzhalter wie bei Kustomize gibt es nicht.
* **Ohne Cluster gibt es kein `lookup`:** `helm template`, `helm install --dry-run=client` und GitOps-Werkzeuge,
  die mit `helm template` rendern (Argo CD), erzeugen bei jedem Rendern neue Werte; jeder Sync beendete dann alle
  Sessions. Dort Session-Key und Admin-Token als eigene Secrets anlegen (etwa aus einem Tresor) und per
  `secrets.sessionKey.existingSecret` und `secrets.adminToken.existingSecret` nennen:

  ```sh
  quackmux token | sed -n 's/^token: *//p' > session-key
  quackmux token | sed -n 's/^token: *//p' > admin-token
  kubectl -n quackmux create secret generic quackmux-session-key-own --from-file=key=session-key
  kubectl -n quackmux create secret generic quackmux-admin-token-own --from-file=token=admin-token
  ```

* `secrets.users.existingSecret` ersetzt den Wert `users` durch ein Secret mit dem Schlüssel `users.yaml`.
* Änderungen: Ein neuer Session-Key beendet alle Sessions (die Clients müssen neu attachen). Session-Key und
  Admin-Token liest das Gateway beim Start; nach einer Änderung an einem vorhandenen Secret
  `kubectl -n quackmux rollout restart deployment/quackmux`. Die Benutzer übernimmt es ohne Neustart (nach etwa
  einer Minute), auch nach `helm upgrade` mit geänderten `users`.

## Hochverfügbarkeit

```sh
helm install quackmux deploy/helm/quackmux -n quackmux -f meine-werte.yaml --set ha.enabled=true
kubectl -n quackmux rollout status deployment/quackmux
```

`ha.enabled` tut, was `overlays/ha` tut: zwei Replikas mit Leader-Election, die HA-Konfiguration, Rollouts mit
`maxSurge: 1` und `maxUnavailable: 0`, das Label `quackmux.io/leader: "true"` im Selektor des Services (und von
`externalService`), ein PodDisruptionBudget mit `minAvailable: 1` und eine bevorzugte `podAntiAffinity`. Beide
Replikas sind ready, Quack-Traffic bekommt nur der Leader. Verhalten bei Übergabe, Ausfall, Rollout und
Node-Wartung: [Hochverfügbarkeit](../../kubernetes/README.md#hochverfügbarkeit-overlaysha).

* **Umstieg eines laufenden Releases:** zuerst das einzelne Gateway stoppen, damit nie ein Gateway ohne
  Leader-Election neben einem Leader läuft. Das Chart prüft das und bricht sonst ab:

  ```sh
  kubectl -n quackmux scale deployment/quackmux --replicas=0
  kubectl -n quackmux wait --for=delete pod -l app.kubernetes.io/component=gateway --timeout=120s
  helm upgrade quackmux deploy/helm/quackmux -n quackmux -f meine-werte.yaml --set ha.enabled=true
  ```

  Zurück ohne HA genügt `helm upgrade` mit `ha.enabled=false`: Die Strategie `Recreate` beendet alle alten Pods,
  bevor der neue startet.
* Ohne Platz für einen dritten Gateway-Pod: `ha.rollingUpdate.maxSurge=0` und `ha.rollingUpdate.maxUnavailable=1`.
* Die Lease (`ha.leaseName`, `ha.namespace`, `provider.kubernetes.instance`) nicht per Upgrade ändern
  ([Lease nicht per Rollout ändern](../../kubernetes/README.md#hochverfügbarkeit-overlaysha)).

## Upgrade

```sh
helm upgrade quackmux deploy/helm/quackmux -n quackmux -f meine-werte.yaml
```

Werte, die `helm upgrade` nicht bekommt, fallen auf die Defaults zurück; deshalb die Werte in einer Datei halten
(oder `--reuse-values`). Ein neues Chart bringt das Gateway-Image seines Releases und dessen Konfiguration mit.
Laufende Engines behalten ihr Image, bis sie enden; ändert sich ihr Profil, ersetzt das Gateway sie
(`engine.replaceOnSpecChange`). `helm rollback` stellt Konfiguration und Image einer früheren Revision wieder her.

## Deinstallieren

```sh
helm -n quackmux uninstall quackmux
```

Das entfernt die Objekte des Charts. Was das Gateway selbst angelegt hat, gehört nicht zum Release und bleibt:

* **Engine-Pods** laufen weiter, bis man sie löscht (ihre Token-Secrets gehen mit ihnen):

  ```sh
  kubectl -n quackmux delete pod -l app.kubernetes.io/component=engine,app.kubernetes.io/managed-by=quackmux
  ```

* **Daten-Volumes persistenter Profile** (PersistentVolumeClaims `quackmux-data-…`) bleiben mit den Daten
  erhalten; ein neues Release im selben Namespace (mit derselben `provider.kubernetes.instance`) findet sie über
  ihre Namen wieder. **Wer sie löscht, löscht die Daten**: `kubectl delete pvc` oder das Löschen des Namespaces
  entfernt alle Claims, mit `reclaimPolicy: Delete` der StorageClass (der Default bei kind und den meisten Clouds)
  auch die Volumes. Für Daten, die bleiben sollen: eine StorageClass mit `reclaimPolicy: Retain` und Backups
  ([Persistente Daten](../../kubernetes/README.md#persistente-daten)).
* Die Lease (mit `ha.enabled`, `quackmux-default`) und der Namespace selbst bleiben ebenfalls;
  `kubectl delete namespace quackmux` entfernt alles, die Daten eingeschlossen.

## Umstieg von Kustomize

Helm übernimmt keine Objekte, die `kubectl apply -k` angelegt hat. Engines und Volumes laufen weiter, das neue
Gateway findet sie wieder (gleiche `provider.kubernetes.instance` und gleicher Namespace). Damit auch die Sessions
überleben, behält man den bisherigen Session-Key:

```sh
kubectl -n quackmux scale deployment/quackmux --replicas=0
kubectl -n quackmux wait --for=delete pod -l app.kubernetes.io/component=gateway --timeout=120s
SESSION_KEY=$(kubectl -n quackmux get deployment quackmux \
  -o jsonpath='{.spec.template.spec.volumes[?(@.name=="session-key")].secret.secretName}')
kubectl -n quackmux delete deployment,service,serviceaccount,role,rolebinding,networkpolicy,poddisruptionbudget,configmap \
  -l app.kubernetes.io/part-of=quackmux
kubectl -n quackmux delete secret -l app.kubernetes.io/part-of=quackmux --field-selector "metadata.name!=$SESSION_KEY"
helm install quackmux deploy/helm/quackmux -n quackmux -f meine-werte.yaml \
  --set secrets.sessionKey.existingSecret="$SESSION_KEY"
```

Den Namespace nicht löschen (`kubectl delete -k` täte das, samt den Daten-Volumes). Die Benutzer kommen aus
`users` (oder `secrets.users.existingSecret`), die Konfiguration aus `configYaml` und `config`; der Admin-Token
ist neu. Mit `overlays/ha` zusätzlich `--set ha.enabled=true`.

## Unterschiede zu Kustomize

CI rendert das Chart mit den Default-Werten und mit `ha.enabled` und vergleicht jedes Objekt mit
`kubectl kustomize deploy/kubernetes/base` bzw. `overlays/ha` ([test/helm](../../../test/helm)). Gleich sein muss
alles außer:

1. **Labels:** Die Objekte des Charts tragen zusätzlich die Helm-Labels `helm.sh/chart`,
   `app.kubernetes.io/instance`, `app.kubernetes.io/version` und `app.kubernetes.io/managed-by: Helm` (nicht am
   Pod-Template: Die Selektoren und Pod-Labels sind dieselben).
2. **Rollout bei neuer Konfiguration:** Das Pod-Template trägt die Annotation `checksum/config`.
3. **Namen ohne Hash:** Kustomize hängt an ConfigMap, Session-Key- und Admin-Token-Secret einen Hash des Inhalts;
   das Chart nicht. Alte ConfigMaps und Secrets sammeln sich deshalb nicht an; zurück geht es mit `helm rollback`
   statt `kubectl rollout undo`.
4. **Secret-Inhalte** (verglichen werden nur die Schlüssel): Kustomize liefert öffentliche Platzhalter, das Chart
   erzeugt Session-Key und Admin-Token und baut `users.yaml` aus den Werten.
5. **Kein Namespace-Objekt:** Das Release kommt in einen vorhandenen Namespace (siehe Installation).

Dazu kommt Verhalten, das Kustomize nicht hat: Die mitgelieferte Konfiguration folgt dem Namespace des Releases,
und das Chart bricht ab bei mehreren Replikas ohne `ha.enabled`, bei einer Replika mit `ha.enabled`, bei
Konfiguration und Wert `ha.enabled`, die nicht zusammenpassen, bei Engines außerhalb des Namespaces des Releases
und beim Umstieg auf HA, solange das Gateway ohne Leader-Election läuft.

Ändert sich `base/config.yaml` oder `overlays/ha/config.yaml`, schlägt `go test ./test/helm/` fehl, bis die
Kopien in `files/` nachgezogen sind:

```sh
go test ./test/helm/ -run TestConfigFilesMatchKustomize -update
```

Die übrigen Prüfungen brauchen `helm` und `kubectl` (Helm etwa per
`go install helm.sh/helm/v3/cmd/helm@v3.22.1`):

```sh
helm lint --strict deploy/helm/quackmux
QUACKMUX_HELM=helm go test -count=1 ./test/helm/
```
