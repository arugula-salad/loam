# Contributing

Pull requests are welcome. For anything bigger than a bug fix or a small feature, open an issue
first to talk it over, so nobody spends a week on a change that doesn't fit.

## Before you open a PR

- `make check` passes (`gofmt`, `go vet`, `go test ./...`). The unit tests need no root, KVM or
  network.
- If you touched the nodelet's VM handling, `make kvm-test` passes on a machine with `/dev/kvm`.
- If you touched `clients/ts`, `npm test` passes there.
- A bug fix comes with a test that fails without it.
- If the change alters behaviour described in a contract (`docs/API.md`, `docs/NODE.md`,
  `docs/AGENT.md`, `docs/E2B.md`, `docs/STORE.md`), the doc and its Go types change in the same
  PR. The user docs (`docs/primer.md`, `docs/reference/`) change with the behaviour they describe.

`CLAUDE.md` has the layout, the build tags and the conventions in more detail. It is written for
coding agents and applies to people just as well.

## Licensing

loam is licensed under Apache-2.0. By submitting a contribution you agree that it is licensed
under the same terms, as section 5 of the license says.
