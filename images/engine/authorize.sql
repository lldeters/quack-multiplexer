-- quackmux_authorize: a quack_authorization_function for the client sessions
-- of a shared engine (deploy/kubernetes/README.md, "Härtung geteilter
-- Engines"). In the engine image: /usr/local/share/quackmux/authorize.sql.
--
-- Load it in the init SQL, before the Quack server starts, and finish the
-- post-start SQL with both settings that DuckDB itself enforces:
--
--   initSQL:      .read /usr/local/share/quackmux/authorize.sql
--   postStartSQL: SET enable_external_access = false;
--                 SET lock_configuration = true;
--
-- The Quack server calls the function for every query of a client (every
-- PREPARE, also the catalog queries of ATTACH) with the connection id and the
-- SQL text, on a connection of its own. The query runs only if the function
-- returns true; an error rejects it and reaches the client as "Authorization
-- callback failed: ...". The engine's own connection (init and post-start
-- SQL, the stop commands of the entrypoint) is not affected.
--
-- Until both settings are in place, every query is rejected: between the
-- start of the server and the end of the post-start SQL the engine is not
-- hardened yet, and without them a session could get around the check (with
-- file access, for example, through the files of the engine's processes in
-- /proc).
--
-- The check looks at the text of a query, not at its meaning, and rejects it
-- when one of these words appears anywhere, also in a string literal or a
-- comment, case-insensitive:
--
-- * quack_* (quack_stop, quack_serve, quack_active_connections,
--   quack_identify, quack_authorization_function, ...) and quackmux_* (these
--   macros);
-- * getenv (the environment holds the token and the init SQL) and
--   shell_history (what the engine's CLI read from its standard input);
-- * query followed by "(", a comment or white space and "(" (the table
--   function query() runs SQL that is built at run time),
--   json_execute_serialized_sql and json_serialize_plan (likewise);
-- * checkpoint and force_checkpoint (FORCE CHECKPOINT aborts the transactions
--   of all sessions), attach and detach (databases that all sessions share);
-- * create or drop followed later by secret, macro or function: secrets and
--   macros are shared by all sessions, a secret supplies the credentials for
--   its scope, a macro takes precedence over a built-in function or an
--   operator of the same name;
-- * schema followed later by system: a schema named "system" would make
--   system.main.<name> below ambiguous, and with it every query fail.
--
-- DuckDB 2.0 has no other spelling of a name: identifiers have no escape
-- sequences, and comments, quotes and Unicode spaces never join two words
-- into one. SQL that is built at run time is rejected as a whole.
--
-- Every built-in function is called as system.main.<name> and no operator is
-- used that DuckDB resolves through the catalog (such as || or LIKE): a macro
-- of the same name in the main schema would take precedence over it. With a
-- database file, both macros are stored in it; CREATE OR REPLACE updates them
-- at every start.

-- quackmux_rejection returns the start of what makes a query rejected, or
-- NULL for a query that may run.
CREATE OR REPLACE MACRO quackmux_rejection(sql) AS
    system.main.nullif(system.main.regexp_extract(sql, system.main.concat(
        '(?:^|[^a-z_])(',
        'quack_\w*\b|quackmux_\w*\b|getenv\b|shell_history\b',
        '|query["`]?(?:[\s\v]|[^\x00-\x7f])*(?:\(|/\*|--)',
        '|json_execute_serialized_sql\b|json_serialize_plan\b',
        '|checkpoint\b|force_checkpoint\b|attach\b|detach\b',
        '|(?:create|drop)\b[\s\S]*[^a-z_](?:secret|macro|function)\b',
        '|schema\b[\s\S]*[^a-z_]system\b',
        ')'), 1, 'i'), '');

-- quackmux_authorize is the authorization function. It returns BOOLEAN:
-- a VARCHAR result would replace the SQL of the query.
CREATE OR REPLACE MACRO quackmux_authorize(connection_id, sql) AS
    CASE
        WHEN system.main.current_setting('enable_external_access')
                OR NOT system.main.current_setting('lock_configuration') THEN
            system.main.error('quackmux_authorize: the engine is not hardened yet (the post-start SQL must set enable_external_access = false and lock_configuration = true); try again')
        WHEN quackmux_rejection(sql) IS NULL THEN
            true
        ELSE
            system.main.error(system.main.concat('quackmux_authorize: not allowed on this engine: "',
                system.main.left(quackmux_rejection(sql), 60), '"'))
    END;

SET GLOBAL quack_authorization_function = 'quackmux_authorize';
