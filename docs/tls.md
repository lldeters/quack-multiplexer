# TLS für den Quack-Port

Ohne `server.tls` spricht das Gateway auf dem Quack-Port Plain HTTP. DuckDB-Clients erwarten für jeden Host außer
`localhost` HTTPS und brauchen dann `DISABLE_SSL true`; Tokens und Daten gehen im Klartext über das Netz. Mit
`server.tls` spricht der Port nur noch HTTPS (ab TLS 1.2), und die Clients verbinden sich ohne `DISABLE_SSL`.

Dem Zertifikat des Gateways vertraut ein Client auf einem von zwei Wegen:

| | CA-Prüfung (Default) | Pinning mit `ssl_fingerprint` |
|---|---|---|
| Vertrauen | CA-Bundle des Systems oder `SET ca_cert_file` | nur der SHA-256-Fingerprint des Zertifikats |
| Hostname und Ablaufdatum | geprüft | nicht geprüft |
| HTTP im Client | httpfs (curl) | eigener HTTP-Client der quack-Extension, ohne httpfs |
| Optionen per `EXTRA_HTTP_HEADERS` | ja | nein, nur im Token |
| Timeout pro Request | 24 h | `http_timeout` (Default 30 s), ohne httpfs 300 s |
| neues Zertifikat | unbemerkt, solange es dieselbe CA ausstellt | jeder Client braucht den neuen Fingerprint |

Wo es geht, ist die CA-Prüfung die bessere Wahl, auch mit einer eigenen CA oder einem selbstsignierten Zertifikat
(`ca_cert_file`, siehe unten). Pinning passt, wenn sich die Clients ein Zertifikat nicht als CA hinterlegen
können oder sollen; dann gelten die Einschränkungen [unten](#was-sich-mit-pinning-ändert).

Das Verhalten der Clients ist gegen DuckDB 2.0.0 (quack `f96420ed46`) mit einem selbstsignierten Zertifikat
geprüft.

## Gateway

```yaml
server:
  tls:
    certFile: /etc/quackmux/tls/tls.crt
    keyFile: /etc/quackmux/tls/tls.key
```

* PEM-Dateien: das Zertifikat (bei einer Kette das Serverzertifikat zuerst) und der unverschlüsselte Schlüssel.
* Das Gateway liest beide nur beim Start. Ein neues Zertifikat gilt erst nach einem Neustart
  (`kubectl -n quackmux rollout restart deployment/quackmux`), dann aber sofort für alle Clients.
* Mit `overlays/ha` mounten alle Replikas dasselbe Secret. Nach einem Wechsel alle neu starten; bis dahin antwortet
  je nach Leader das alte oder das neue Zertifikat.
* Kubernetes: Secret und Volume-Patch stehen in [deploy/kubernetes/README.md](../deploy/kubernetes/README.md#von-außen-l4-load-balancer).
  Terminiert stattdessen ein L4-Load-Balancer TLS, sehen die Clients sein Zertifikat; ein Pin gilt dann diesem.

Ein selbstsigniertes Zertifikat (OpenSSL 1.1.1 oder neuer; der Name muss der sein, den die Clients im `ATTACH`
verwenden):

```sh
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 730 \
  -subj '/CN=quackmux.example.com' -addext 'subjectAltName=DNS:quackmux.example.com' \
  -keyout tls.key -out tls.crt
kubectl -n quackmux create secret tls quackmux-tls --cert=tls.crt --key=tls.key
```

## Clients mit CA-Prüfung

```sql
CREATE SECRET (TYPE quack, TOKEN 'mein-token', SCOPE 'quack:quackmux.example.com:9494');
ATTACH 'quack:quackmux.example.com:9494' AS lake;
```

Für ein Zertifikat, das keine öffentliche CA ausgestellt hat, braucht der Client vorher das CA-Zertifikat, bei
einem selbstsignierten das Zertifikat selbst:

```sql
SET ca_cert_file = '/pfad/zu/ca.crt';
```

Der Host im `ATTACH` muss im Zertifikat stehen (`subjectAltName`). Sonst und ohne passende CA scheitert das `ATTACH`
mit `SSL peer certificate or SSH remote key was not OK`. Optionen per `EXTRA_HTTP_HEADERS` funktionieren auf diesem
Weg wie ohne TLS.

## Clients mit `ssl_fingerprint`

### Fingerprint ermitteln

Aus der Zertifikatsdatei, aus dem Secret im Cluster oder vom laufenden Gateway (zeigt, was die Clients sehen):

```sh
openssl x509 -in tls.crt -noout -fingerprint -sha256 | cut -d= -f2

kubectl -n quackmux get secret quackmux-tls -o jsonpath='{.data.tls\.crt}' | base64 -d |
  openssl x509 -noout -fingerprint -sha256 | cut -d= -f2

openssl s_client -connect quackmux.example.com:9494 -servername quackmux.example.com </dev/null 2>/dev/null |
  openssl x509 -noout -fingerprint -sha256 | cut -d= -f2
```

Das Ergebnis hat die Form `F8:C0:31:…:BB:7D` (64 Hex-Ziffern). DuckDB nimmt die Ziffern mit oder ohne
Doppelpunkte oder Leerzeichen, in beliebiger Schreibweise und optional mit dem Präfix `sha256:`. Die ganze
Ausgabezeile von `openssl` (`sha256 Fingerprint=…`) lehnt es ab, deshalb das `cut`.

### Pin setzen

Im Secret, dann gilt der Pin für jedes `ATTACH` auf den Scope:

```sql
CREATE SECRET quackmux (TYPE quack, TOKEN 'mein-token', SSL_FINGERPRINT 'F8:C0:31:…:BB:7D',
                        SCOPE 'quack:quackmux.example.com:9494');
ATTACH 'quack:quackmux.example.com:9494' AS lake;
```

oder als Option des `ATTACH`, die einen Pin im Secret ersetzt:

```sql
ATTACH 'quack:quackmux.example.com:9494' AS lake (SSL_FINGERPRINT 'sha256:f8c031…bb7d');
```

Ein Pin schaltet HTTPS ein. Steht er im `ATTACH` oder in einem dort mit `SECRET` genannten Secret, lehnt DuckDB
`DISABLE_SSL true` ab; kommt er aus einem Secret, das DuckDB über seinen `SCOPE` findet, gilt er, und DuckDB
übergeht `DISABLE_SSL` ohne Meldung.

### Was sich mit Pinning ändert

* **Nur der Fingerprint zählt.** Der Client prüft weder CA noch Hostnamen noch Ablaufdatum; auch ein
  abgelaufenes Zertifikat nimmt er an. Wer den Schlüssel hat, kann sich als Gateway ausgeben, bis die Clients
  einen neuen Pin haben.
* **Kein httpfs.** Der Client spricht mit einem eigenen HTTP-Client der quack-Extension und lädt httpfs dafür
  nicht.
* **Keine `EXTRA_HTTP_HEADERS`.** Dieser Client sendet keine Zusatz-Header; `X-Quackmux-*`-Header im Secret
  ignoriert er ohne Meldung, und die Session landet auf den Defaults. Optionen gehören in den Token:
  `TOKEN 'mein-token?subdomain=bi&share_level=connection'`.
* **Kurze Timeouts.** Jeder Request wartet höchstens `http_timeout` Sekunden (Default 30), sobald im
  Client-Prozess httpfs geladen ist (etwa für S3, durch `LOAD httpfs` oder eine httpfs-Einstellung), sonst 300 s.
  Lange warten aber gerade die Quack-Requests: das `ATTACH`, während das Gateway eine Engine startet
  (`CONNECTION` ohne Warm-Pool, die erste Session eines Engine-Schlüssels; bis `engine.startupTimeout`), und ein
  FETCH, bis die Engine die nächsten Zeilen hat (eine Aggregation liefert erst am Ende). Dauert es länger, meldet der
  Client `Failed to read connection error for HTTP POST to 'https://…/quack'`; die Session bleibt nutzbar. Ohne
  Pinning wartet der Client bis zu 24 h. Abhilfe vor dem `ATTACH`:

  ```sql
  SET GLOBAL http_timeout = 86400;
  ```

  Nur `GLOBAL` wirkt: Ein `SET http_timeout` ohne `GLOBAL` gilt für die Sitzung, der gepinnte Client liest aber den
  globalen Wert. Die Einstellung lädt httpfs und gilt auch für dessen eigene Requests (S3, HTTPS) im Prozess. Mit
  ihr wartet ein gepinnter Client auch einen kalten Engine-Start ab; einen Warm-Pool braucht er dafür nicht.
* **Zertifikatswechsel.** Jeder Client braucht den neuen Fingerprint, sobald das Gateway das neue Zertifikat
  ausliefert, also mit dem nächsten Neustart, auch einem ungeplanten (Rollout, Failover, Node-Ausfall). Ein
  automatisch erneuertes Zertifikat (cert-manager) passt deshalb schlecht zum Pinning: besser ein langlebiges
  selbstsigniertes Zertifikat, dessen Wechsel man mit den Clients abstimmt.

## Fehlermeldungen

| Meldung des Clients | Ursache |
|---|---|
| `IO Error: Failed to send message: the certificate presented by https://…:9494 does not match the pinned ssl_fingerprint` | falscher Pin, oder das Gateway liefert ein neues Zertifikat |
| `Invalid Input Error: Invalid SSL fingerprint "…": expected the SHA-256 fingerprint of the server certificate as hex digits, optionally separated by colons` | Pin im falschen Format, etwa die ganze `openssl`-Zeile |
| `Invalid Input Error: ssl_fingerprint pins the server's TLS certificate and cannot be combined with disable_ssl` | Pin im `ATTACH` und `DISABLE_SSL true` |
| `IO Error: Failed to send message: Failed to read connection error for HTTP POST to 'https://…:9494/quack'` | gepinnter Client: Request dauerte länger als sein Timeout |
| `IO Error: Failed to send message: IO Error: SSL peer certificate or SSH remote key was not OK error for HTTP POST to 'https://…:9494/quack'` | ohne Pin: Zertifikat nicht vertrauenswürdig oder Hostname passt nicht |
| `IO Error: Failed to send message: Bad Request` | `DISABLE_SSL true` gegen ein Gateway mit `server.tls` |
