# Contributing

Keep changes focused and solve the problem with the least machinery needed.
Read [AGENTS.md](AGENTS.md) for the code map and contracts, and
[the design](docs/contributing/plan.md) for why things are built the way they are.

## Setup and verification

Use Go 1.26.10 or newer, Python 3.9 or newer (standard library only), and a POSIX
shell on Linux or macOS.

```sh
./x check
./x release-check
```

| Command | Purpose |
| --- | --- |
| `./x fmt` | Format all Go source |
| `./x fmt --check` | Check formatting without editing |
| `./x test` | Run Go tests with the race detector and the Python script tests |
| `./x setup [FLAGS]` | Build and preview setup in a temporary home, with SSH disabled |
| `./x try [TEST]` | Build and open the test menu, or run one named check |
| `./x bench` | Run the benchmarks (microbenchmarks and the end-to-end typing-latency benchmark) |
| `./x docs` | Check guide metadata and local Markdown links |
| `./x servers [VERSION]` | Cross-compile and gzip the servers; packaging passes its exact release version |
| `./x build` | Cross-compile the embedded servers, then build `bin/aft` |
| `./x check` | Formatting, vet, tests, docs and build |
| `./x audit` | Check reachable vulnerabilities with the active Go toolchain and dependency graph |
| `./x release-check [vX.Y.Z]` | Build all archives, inspect metadata and exercise the native artifact in a temporary directory |
| `./x dist vX.Y.Z` | Write archives, checksums and notes to `dist/vX.Y.Z`; never publish |

A plain `go build ./cmd/aft` works for development but embeds no servers, so it
can only use the host `local`. Run `./x build` for a binary that can install
itself on remote hosts.

Third-party dependencies are allowed when they earn their place; each new one
is justified in review.

## Contributions and review

Bug reports should include `aft version`, both operating systems, the terminal
application, reproducible steps, and expected and actual behavior. Sanitize
hostnames, paths and logs before sharing. Report vulnerabilities through
[the security policy](SECURITY.md), not public issues.

Explain the problem and resulting behavior in your PR, record check results,
add focused regression tests when behavior changes, update affected guides, and
summarize user-visible changes in [Unreleased](CHANGELOG.md). Use a clear title
such as `fix(client): repaint stale predictions after resize`.

Contributions are licensed under the [MIT license](LICENSE).

## Dependency updates

Dependabot groups dependency and GitHub Actions updates into one weekly pull request. Review changes and required checks before merging.
