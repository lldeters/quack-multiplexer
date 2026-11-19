-- quackmux: Beispiele für DuckDB-2.0-Clients
--
-- Blöcke einzeln in die DuckDB-Shell kopieren oder alles ausführen:
--   duckdb < examples/client.sql
--
-- Annahmen:
--   * Das Gateway ist unter localhost:9494 erreichbar, in Kubernetes z. B. per
--       kubectl -n quackmux port-forward svc/quackmux 9494:9494
--   * Konfiguration wie in deploy/kubernetes/base: Benutzer alice (Gruppe
--     analytics) mit dem öffentlichen Beispiel-Token
--     'change-me-example-token-0000', Profile default und large.
--   * Lokal mit examples/config.local.yaml (auth.mode=none) ist jedes Token
--     gültig, der Benutzer kommt aus der Option user (siehe Abschnitt 7).
--     Dort gibt es nur das Profil default und die Gruppe dev.

-- Falls die Extension nicht automatisch geladen wird:
INSTALL quack;
LOAD quack;


-- 1. Verbinden: Secret anlegen, attachen, abfragen, trennen -----------------

-- Das Secret liefert das Token für jedes ATTACH, dessen Ziel zum SCOPE passt.
-- Mit einem Namen lässt es sich später per CREATE OR REPLACE ersetzen.
CREATE OR REPLACE SECRET quackmux (
    TYPE quack,
    TOKEN 'change-me-example-token-0000',
    SCOPE 'quack:localhost:9494'
);

-- localhost spricht der Client per HTTP an; andere Hosts per HTTPS, außer mit
-- (DISABLE_SSL true), siehe Abschnitt 5. Ohne Optionen gilt der Default der
-- Konfiguration: share_level=user, also eine eigene Engine für alice.
ATTACH 'quack:localhost:9494' AS lake;

-- Eine Query in der Engine ausführen:
FROM lake.query('SELECT 42 AS antwort, version() AS duckdb_version');

-- Tabellen der Engine-Datenbank wie lokale Tabellen ansprechen. Die Datenbank
-- der Engine liegt im Speicher (":memory:"): Sie lebt, bis die Engine nach
-- engine.idleTimeout ohne Sessions beendet wird.
CREATE OR REPLACE TABLE lake.main.messwerte AS
    SELECT range AS id, random() AS wert FROM range(1000);
SELECT count(*) AS anzahl, round(avg(wert), 3) AS mittel FROM lake.main.messwerte;

-- DETACH schickt DISCONNECT, das Gateway schließt die Session. Die Engine
-- bleibt für weitere Sessions von alice bestehen.
DETACH lake;


-- 2. Share-Levels über Token-Optionen ---------------------------------------
--
-- Optionen hängen mit '?' am Token: TOKEN?option=wert&option=wert. Das
-- funktioniert mit jedem Client-Transport.
--   share_level  connection | user (Default) | group | server
--   subdomain    getrennte Engines je Owner ([a-z0-9-], max. 32 Zeichen)
--   profile      Engine-Profil aus der Konfiguration (Default: default)
--   group        Gruppe für share_level=group (Default: erste Gruppe)
--   user         nur bei auth.mode=none: der Benutzername
--
-- Ein Secret gilt für alle ATTACHes auf denselben SCOPE. Für andere Optionen
-- das Secret ersetzen und neu attachen; bereits attachte Datenbanken behalten
-- ihre Session und Engine.

-- 2a. CONNECTION: ein eigener Pod nur für dieses ATTACH. Er wird beim DETACH
--     beendet. Volle Isolation, kostet aber einen Pod-Start (ein warmPool im
--     Profil hält Engines vorgestartet).
CREATE OR REPLACE SECRET quackmux (
    TYPE quack,
    TOKEN 'change-me-example-token-0000?share_level=connection',
    SCOPE 'quack:localhost:9494'
);
ATTACH 'quack:localhost:9494' AS privat;
FROM privat.query('SELECT current_setting(''threads'') AS threads');
DETACH privat;

-- 2b. Profil large (4 CPUs, 16 GiB; nur mit USER oder CONNECTION erlaubt).
--     Auskommentiert, weil der Pod einen entsprechend großen Node braucht.
-- CREATE OR REPLACE SECRET quackmux (
--     TYPE quack,
--     TOKEN 'change-me-example-token-0000?share_level=connection&profile=large',
--     SCOPE 'quack:localhost:9494'
-- );
-- ATTACH 'quack:localhost:9494' AS gross;
-- FROM gross.query('SELECT current_setting(''memory_limit'') AS memory_limit');
-- DETACH gross;

-- 2c. GROUP: eine Engine für alle Mitglieder der Gruppe analytics. Die
--     Mitglieder teilen den Datenbankzustand: Was alice anlegt, sieht auch bob.
CREATE OR REPLACE SECRET quackmux (
    TYPE quack,
    TOKEN 'change-me-example-token-0000?share_level=group&group=analytics',
    SCOPE 'quack:localhost:9494'
);
ATTACH 'quack:localhost:9494' AS team;
CREATE TABLE IF NOT EXISTS team.main.notizen (von VARCHAR, text VARCHAR);
INSERT INTO team.main.notizen VALUES ('alice', 'Hallo Team');
FROM team.main.notizen;
DETACH team;

-- 2d. SERVER: eine Engine für alle Benutzer (je Subdomain und Profil).
CREATE OR REPLACE SECRET quackmux (
    TYPE quack,
    TOKEN 'change-me-example-token-0000?share_level=server',
    SCOPE 'quack:localhost:9494'
);
ATTACH 'quack:localhost:9494' AS alle;
FROM alle.query('SELECT 1 AS ok');
DETACH alle;


-- 3. Subdomains: mehrere getrennte Engines je Owner -------------------------
--
-- Beide Engines gehören alice (USER), teilen aber keinen Zustand, z. B. eine
-- für ETL-Jobs und eine für Ad-hoc-Abfragen.
CREATE OR REPLACE SECRET quackmux (
    TYPE quack,
    TOKEN 'change-me-example-token-0000?subdomain=etl',
    SCOPE 'quack:localhost:9494'
);
ATTACH 'quack:localhost:9494' AS etl;
CREATE OR REPLACE TABLE etl.main.staging AS SELECT * FROM range(10) t(n);
DETACH etl;

CREATE OR REPLACE SECRET quackmux (
    TYPE quack,
    TOKEN 'change-me-example-token-0000?subdomain=adhoc',
    SCOPE 'quack:localhost:9494'
);
ATTACH 'quack:localhost:9494' AS adhoc;
-- Würde scheitern: staging existiert nur in der Engine der Subdomain etl.
-- FROM adhoc.main.staging;
FROM adhoc.query('SELECT 1 AS ok');
DETACH adhoc;


-- 4. Optionen als HTTP-Header (EXTRA_HTTP_HEADERS) --------------------------
--
-- Dieselben Optionen wie im Token: X-Quackmux-Share-Level, X-Quackmux-Subdomain,
-- X-Quackmux-Profile, X-Quackmux-Group. Priorität: Token-Optionen > Header >
-- overrides der Konfiguration > defaults. Header sendet der Client nur über
-- den httpfs-Transport, nicht bei Fingerprint-Pinning (ssl_fingerprint); dort
-- Token-Optionen verwenden.
CREATE OR REPLACE SECRET quackmux (
    TYPE quack,
    TOKEN 'change-me-example-token-0000',
    SCOPE 'quack:localhost:9494',
    EXTRA_HTTP_HEADERS MAP {
        'X-Quackmux-Share-Level': 'server',
        'X-Quackmux-Subdomain': 'bi'
    }
);
ATTACH 'quack:localhost:9494' AS bi;
FROM bi.query('SELECT 1 AS ok');
DETACH bi;


-- 5. Entferntes Gateway ------------------------------------------------------
--
-- Ohne server.tls spricht das Gateway Plain HTTP. Für andere Hosts als
-- localhost dann DISABLE_SSL setzen (nur in vertrauenswürdigen Netzen):
-- CREATE OR REPLACE SECRET quackmux (
--     TYPE quack,
--     TOKEN 'mein-token',
--     SCOPE 'quack:quackmux.example.com:9494'
-- );
-- ATTACH 'quack:quackmux.example.com:9494' AS lake (DISABLE_SSL true);
--
-- Mit TLS (server.tls oder ein L4-Load-Balancer, der TLS terminiert) ohne
-- DISABLE_SSL; das Zertifikat muss für den Client vertrauenswürdig sein:
-- ATTACH 'quack:quackmux.example.com:9494' AS lake;


-- 6. Typische Fehlermeldungen -------------------------------------------------
--
--   Authentication failed
--       Token unbekannt oder abgelaufen, Benutzer gesperrt, oder der SCOPE des
--       Secrets passt nicht zum Ziel des ATTACH.
--   quackmux: session closed: idle timeout (6h0m0s). DETACH and ATTACH again to reconnect.
--       Keine Query länger als session.idleTimeout (Heartbeats zählen nicht).
--   quackmux: not permitted: ...
--       Share-Level, Profil oder Gruppe für diesen Benutzer nicht erlaubt.
--   quackmux: could not provide an engine: ...
--       Limit erreicht oder Engine-Start fehlgeschlagen (Gateway-Log und
--       kubectl -n quackmux describe pod quack-... prüfen).
--
-- Nach "session closed" das Attachment erneuern:
-- DETACH lake;
-- ATTACH 'quack:localhost:9494' AS lake;


-- 7. Entwicklung ohne Authentifizierung (auth.mode=none) --------------------
--
-- examples/config.local.yaml und das Kubernetes-Overlay dev akzeptieren jedes
-- Token; der Benutzer kommt aus der Option user (Default: dev).
-- CREATE OR REPLACE SECRET quackmux (
--     TYPE quack,
--     TOKEN 'dev?user=alice&share_level=connection',
--     SCOPE 'quack:localhost:9494'
-- );
-- ATTACH 'quack:localhost:9494' AS lake;


-- Aufräumen: das Secret entfernen.
DROP SECRET quackmux;
