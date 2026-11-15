# hospitium-agent: notes for AI assistants

A Raspberry Pi agent that keeps Google TVs at a desired state over ADB, pulled from a control plane
(`hospitium-protocol`) or a local file (standalone mode). Read `README.md` and `docs/google-tv.md`.
`CONTRIBUTING.md` applies to you too.

## Ground rules

- **This repo is public.** Never reference private repositories, deployments, hostnames, infrastructure,
  customers, properties or secrets paths, and never name the operator's other products. Examples use
  `.example` domains and RFC 1918 addresses.
- **The agent only manages TVs.** No guest interface, reservations, guest data or smart-home control here.
- **Closed action set.** `tv.Device` has no generic shell method and the protocol has no shell operation.
  Every new TV action is a named, reviewed method. Shell arguments are always quoted (`shellQuote`).
- **Observe, diff, act, verify.** Reconciliation reads first and changes only what differs; a compliant TV
  receives read-only commands (tested). Never clear app data during reconciliation.
- **Operations are at-least-once.** Write the durable record before executing; re-report instead of
  re-executing; refuse expired operations.
- **Context flows end to end**; logging is `log/slog` `*Context` variants only. Lint enforces both.
- **Label evidence honestly.** Anything learned from the simulator is `[sim]`, not hardware behavior.
- **Every Go file starts with** `// SPDX-License-Identifier: Apache-2.0`.
- **Dependencies:** standard library only today; justify any new module.

## Releases

- **Only through `task release BUMP=patch|minor|major`, and only when the maintainer asks.** Never
  `git tag` or `git push --tags` by hand. A tag publishes binaries that hubs in the field install.
- The task tags the tip of `origin/main` after CI passed on it and the commit builds from published
  modules only. Release the protocol first (its own `task release`), then require that tag here.
- `release:next` is the only copy of the version arithmetic; `task release:check` self-tests it.

## Commands

All recurring commands live in `Taskfile.yaml` (`task --list`). `task check` must pass with no network.
