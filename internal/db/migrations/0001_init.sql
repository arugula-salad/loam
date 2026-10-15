-- loam-api's records. A sandbox's state is what loam-api last knew of its VM;
-- the nodelet's own is reconciled against it.

CREATE TABLE projects (
    name       text PRIMARY KEY,
    created_at timestamptz NOT NULL
);

-- Keys are stored only as the SHA-256 of their secret.
CREATE TABLE api_keys (
    id         text PRIMARY KEY,
    project    text NOT NULL REFERENCES projects (name) ON DELETE CASCADE,
    name       text NOT NULL,
    hash       bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL,
    UNIQUE (project, name)
);

-- ON DELETE RESTRICT: a project with sandboxes can't be deleted (409 project_not_empty).
CREATE TABLE sandboxes (
    id              text PRIMARY KEY,
    project         text NOT NULL REFERENCES projects (name) ON DELETE RESTRICT,
    name            text NOT NULL,
    template        text NOT NULL,
    state           text NOT NULL,
    error           text NOT NULL DEFAULT '',
    node            text NOT NULL DEFAULT '',
    cpus            integer NOT NULL,
    memory_mib      integer NOT NULL,
    disk_gib        integer NOT NULL,
    env             jsonb NOT NULL DEFAULT '{}',
    labels          jsonb NOT NULL DEFAULT '{}',
    idle_timeout_s  integer NOT NULL,
    idle_action     text NOT NULL,
    deadline_at     timestamptz,
    deadline_action text,
    protected       boolean NOT NULL DEFAULT false,
    http_port       integer NOT NULL,
    next_checkpoint integer NOT NULL DEFAULT 0,
    next_auto       integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    last_active_at  timestamptz NOT NULL,
    UNIQUE (project, name)
);

CREATE TABLE checkpoints (
    sandbox    text NOT NULL REFERENCES sandboxes (id) ON DELETE CASCADE,
    id         text NOT NULL,
    comment    text NOT NULL DEFAULT '',
    memory     boolean NOT NULL,
    auto       boolean NOT NULL,
    created_at timestamptz NOT NULL,
    seq        bigserial,
    PRIMARY KEY (sandbox, id)
);

CREATE TABLE port_settings (
    sandbox text NOT NULL REFERENCES sandboxes (id) ON DELETE CASCADE,
    port    integer NOT NULL,
    access  text NOT NULL,
    PRIMARY KEY (sandbox, port)
);

-- Events are best effort (docs/API.md, "Events"); kept a day for a client
-- that reconnects soon after, then pruned.
CREATE TABLE events (
    id      bigserial PRIMARY KEY,
    project text NOT NULL,
    sandbox text NOT NULL,
    type    text NOT NULL,
    at      timestamptz NOT NULL,
    detail  jsonb
);
CREATE INDEX events_project_at ON events (project, at);
