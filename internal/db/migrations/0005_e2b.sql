-- Phase 4 (docs/E2B.md): what the E2B adapter knows of a sandbox it made,
-- beyond the sandbox's own record. The row goes with the sandbox, however it
-- is deleted.
--
-- sealed_token is envd's access token, handed to it at every /init and back
-- to the SDK on create, connect and get, so it has to be kept whole: it is
-- AES-256-GCM under loam-api's secret key, nonce first, sealed with the
-- sandbox's ID as additional data, so a ciphertext copied into another row
-- doesn't open. It is never logged.
CREATE TABLE e2b_sandboxes (
    sandbox      text PRIMARY KEY REFERENCES sandboxes (id) ON DELETE CASCADE,
    project      text NOT NULL,
    template     text NOT NULL,
    sealed_token bytea NOT NULL,
    timeout_s    integer NOT NULL,
    started_at   timestamptz NOT NULL,
    end_at       timestamptz NOT NULL,
    auto_pause   boolean NOT NULL,
    keep_memory  boolean NOT NULL,
    auto_resume  boolean NOT NULL,
    paused       boolean NOT NULL
);
CREATE INDEX e2b_sandboxes_project ON e2b_sandboxes (project);
