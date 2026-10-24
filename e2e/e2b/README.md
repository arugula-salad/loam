
Run `node templates.mjs` with the same `E2B_*` environment against an isolated
nodelet to exercise SDK file uploads, versioned template builds, readiness,
process-preserving snapshot creates, default user/workdir, environment, and fresh
sandbox identity. It deletes the sandboxes and template after the test. Allow one
build sandbox plus the forked instances; this test creates retained checkpoints.

Set `LOAM_LARGE_COPY=1` when running `node templates.mjs` to upload an archive
containing 60 MiB of random bytes and 100 MiB of zeros, then verify both SHA-256
hashes after each snapshot create. Run this against an isolated deployment with
Postgres constrained to 512 MiB to reproduce the large-COPY regression safely.
