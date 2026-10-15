-- Phase 2 (docs/NODE.md, "Phase 2: the snapshot store"): every snapshot is
-- recorded, checkpoints' and pauses' and stops' alike, with the layers it
-- references in the store, so that layers can be refcounted and a sandbox can
-- wake on another node from its newest snapshot.

-- snapshot: the sandbox's newest snapshot (snapshots.id), '' for none.
-- next_snapshot numbers its <id>.@<n> pause and stop snapshots.
ALTER TABLE sandboxes
    ADD COLUMN snapshot      text NOT NULL DEFAULT '',
    ADD COLUMN next_snapshot integer NOT NULL DEFAULT 0;

-- No foreign key to sandboxes: a snapshot outlives its sandbox while a fork
-- starts from it. Garbage collection (internal/db, GarbageSnapshots) decides.
CREATE TABLE snapshots (
    id         text PRIMARY KEY,
    sandbox    text NOT NULL,
    kind       text NOT NULL,
    checkpoint text NOT NULL DEFAULT '',
    node       text NOT NULL,
    parent     text NOT NULL DEFAULT '',
    layers     text[] NOT NULL DEFAULT '{}',
    memory     boolean NOT NULL,
    stored     boolean NOT NULL DEFAULT false,
    state      text NOT NULL,
    size_bytes bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL,
    seq        bigserial
);
CREATE INDEX snapshots_sandbox ON snapshots (sandbox, seq);
CREATE INDEX snapshots_node ON snapshots (node, seq);
CREATE INDEX snapshots_layers ON snapshots USING gin (layers);
CREATE INDEX sandboxes_snapshot ON sandboxes (snapshot);

-- Every layer a record has referenced and that isn't deleted yet; the ones no
-- record references any more are deleted from the store.
CREATE TABLE layers (
    id         text PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE vm_tombstones (
    node       text NOT NULL,
    vm         text NOT NULL,
    deleted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node, vm)
);

-- Phase 1's checkpoints are snapshots on their sandbox's node, never stored and
-- with no layers known: record them so that they are deleted like any other.
INSERT INTO snapshots (id, sandbox, kind, checkpoint, node, memory, stored, state, created_at)
SELECT c.sandbox || '.' || c.id, c.sandbox, 'checkpoint', c.id, s.node, c.memory, false, 'ready', c.created_at
FROM checkpoints c JOIN sandboxes s ON s.id = c.sandbox
ORDER BY c.seq;
