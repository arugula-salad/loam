-- Uploaded archives are a cache, not part of a completed snapshot. Discard the
-- old inline cache without decoding it; SDKs will upload missing files again.
UPDATE e2b_templates SET data = data - 'files' WHERE data ? 'files';
CREATE TABLE e2b_template_file_chunks (
    template text NOT NULL REFERENCES e2b_templates(id) ON DELETE CASCADE,
    hash text NOT NULL,
    sequence integer NOT NULL,
    data bytea NOT NULL CHECK (octet_length(data) <= 1048576),
    PRIMARY KEY (template, hash, sequence)
);
