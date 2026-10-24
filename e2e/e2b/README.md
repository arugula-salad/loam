
Run `node templates.mjs` with the same `E2B_*` environment against an isolated
nodelet to exercise SDK file uploads, versioned template builds, readiness,
process-preserving snapshot creates, default user/workdir, environment, and fresh
sandbox identity. It deletes the sandboxes and template after the test. Allow one
build sandbox plus the forked instances; this test creates retained checkpoints.
