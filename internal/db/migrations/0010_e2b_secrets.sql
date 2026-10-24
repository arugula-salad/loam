CREATE TABLE e2b_secrets (
 id text PRIMARY KEY,
 project text NOT NULL REFERENCES projects(name) ON DELETE CASCADE,
 name text NOT NULL,
 data jsonb NOT NULL,
 UNIQUE(project,name)
);
