# Fountain's pinned Claude ACP installation

The image installs `@agentclientprotocol/claude-agent-acp@0.81.2` under
`/home/sprite/.local/share/managoat/acp/claude-agent-acp@0.81.2`, with its locked
JavaScript dependency tree (Claude SDK 0.3.280), `.installed`, `.node`,
`.claude-executable`, executable `launch`, and the PATH symlink expected by Fountain.

The TSV is copied unchanged from `managoat/fountain` at
`94091d95959841141d235b22df6d2080e13ec7ff`, path
`apps/fountain/priv/runtimes/acp/claude-agent-acp@0.81.2.tsv` (Git blob
`973032d780a6f59bc08e9985ba6267b1a96721fa`). Every tarball is SHA-512 verified
before extraction. No dependency is resolved from a floating npm range.

The launcher and marker layout follow Fountain's installation contract; both its
original manifest installer and current bundle installer's cache check accept it.
The installer selects the image's Claude Code executable (minimum 2.1.280), records
its resolved path, and skips the SDK's optional native CLI packages. This matches
Fountain's current image-reuse contract and avoids a second CLI in the cache.

When Fountain changes its adapter pin, copy its reviewed manifest, update VERSION
and the Containerfile filename together, and verify the launch entry against its
runtime table. The change rebuilds the image through images.yml. Until that image
ships, Fountain's versioned directory lookup misses and uses its normal runtime
installation fallback. The weekly CLI refresh does not change this dependency pin.

Before merging, create a fresh fountain sandbox, run Fountain's cache check and ACP
initialize/session smoke test, and run the live adapter workflow. Record provisioning
time before and after; the preinstalled version must require no package downloads.
