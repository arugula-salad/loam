-- Phase 3 (docs/API.md, "Network" and "Secrets"): what a sandbox may reach,
-- and the project secrets it uses.

-- network: the rules as the client gave them, includes unexpanded, so that a
-- later version's defaults apply at the next wake. secrets: the names of the
-- project secrets the sandbox uses; an array so that "who names this secret"
-- is an index lookup.
ALTER TABLE sandboxes
    ADD COLUMN network jsonb NOT NULL DEFAULT '{"egress": []}',
    ADD COLUMN secrets text[] NOT NULL DEFAULT '{}';
CREATE INDEX sandboxes_secrets ON sandboxes USING gin (secrets);

-- value is AES-256-GCM under loam-api's secret key, nonce first, sealed with
-- the project and the name as additional data, so a ciphertext copied into
-- another row doesn't open. Nothing else here is secret.
CREATE TABLE secrets (
    project     text NOT NULL REFERENCES projects (name) ON DELETE CASCADE,
    name        text NOT NULL,
    hosts       text[] NOT NULL,
    placeholder text NOT NULL,
    inject      jsonb,
    value       bytea NOT NULL,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (project, name)
);
