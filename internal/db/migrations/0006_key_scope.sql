-- A key's scope (docs/API.md, "Authentication and projects"): 'full', what
-- every key was, or 'read', which may only read (a log shipper's).
ALTER TABLE api_keys ADD COLUMN scope text NOT NULL DEFAULT 'full';
