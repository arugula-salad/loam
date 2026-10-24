CREATE TABLE e2b_templates (
    id text PRIMARY KEY,
    project text NOT NULL REFERENCES projects(name) ON DELETE CASCADE,
    name text NOT NULL,
    data jsonb NOT NULL,
    UNIQUE (project, name)
);

ALTER TABLE e2b_sandboxes ADD COLUMN default_user text NOT NULL DEFAULT '';
ALTER TABLE e2b_sandboxes ADD COLUMN default_workdir text NOT NULL DEFAULT '';
