# Security

loam runs untrusted code in Firecracker microVMs, so its isolation is the point of it. Reports
of a sandbox escape are the most valuable thing you can send, and so are ways for a sandbox to
reach the node, the pod network or another sandbox, to get around its egress rules, to read a
brokered secret's real value, to obtain another sandbox's identity token, or to use another
project's sandboxes.

## Reporting

Please report vulnerabilities privately, through GitHub's
[private vulnerability reporting](https://github.com/arugula-salad/loam/security/advisories/new)
for this repository. Don't open a public issue or pull request for them.

Include what you found, how to reproduce it, and the commit or image you tested. You'll get an
acknowledgement within a few days. Once a fix is out, the advisory is published with credit to
you unless you'd rather stay anonymous.

## Scope

In scope is the code in this repository and the container images built from it. Known
limitations are documented and out of scope as vulnerabilities, in particular that an
unprivileged nodelet's VMs share the pod's cgroup (`README.md`, "Status and limitations").
Bugs in Firecracker, the Linux kernel or envd belong upstream, though we'd like to hear about
them if loam makes them worse.
