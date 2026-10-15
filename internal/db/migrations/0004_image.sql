-- Phase 4 (docs/API.md, "image" on create): a sandbox made from an OCI image
-- rather than a template keeps the image's digest here, and its template is
-- ''. The digest is all a node needs to build its disk again on a move.
ALTER TABLE sandboxes ADD COLUMN image text NOT NULL DEFAULT '';
