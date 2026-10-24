# Guest kernel

The nodelet builds Linux 6.1.155 from the kernel.org source archive, verified by
SHA-256 in the Containerfile. `kernel.config` is extracted from the previously shipped Firecracker CI artifact:
`https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155`
(SHA-256 `e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2`),
using Linux's `scripts/extract-ikconfig`. This preserves the actual deployed
boot configuration rather than a different source-tree sample.
The build enables config introspection and built-in nftables inet filtering,
TCP-reset rejection, counters, and iptables UID-owner matching. No guest module
installation is needed. The host-side nodelet firewall remains separate.

Update the source version/checksum and review the base configuration together.
Run `make kvm-test`; its guest UID-firewall case checks IPv4 and IPv6 loopback
rejection for UID 1002 while another UID and other ports remain reachable, then
checks that removing the rule restores access. The normal snapshot/fork tests
also run against the rebuilt kernel.

A nodelet rollout changes the kernel used on fresh boot. Already-running VMs and
memory snapshots retain their old kernel. Rebuild running templates and create
new sandboxes (or stop and fresh-boot existing sandboxes) to use the new features;
pause/resume alone does not upgrade the guest kernel.
