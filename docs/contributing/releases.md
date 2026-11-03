# Releases

The first official release is **v0.1.0**. Preparing a branch, building local
archives or merging a PR does not publish a release. Publication status and
available downloads are on [GitHub Releases](https://github.com/fschrhunt/aft/releases).

## v0.1.0 qualification

This is an initial 0.x release, with no promise of a frozen CLI or configuration
format. The release notes are the exact `## 0.1.0` section of
[CHANGELOG.md](../../CHANGELOG.md).

Evidence gathered on 2026-10-25:

| Check | Evidence or remaining work |
| --- | --- |
| Linux and macOS source CI | Passed on merged PR #16; [main CI run](https://github.com/fschrhunt/aft/actions/runs/40148257486) |
| Archive versions, embedded servers, platform metadata and checksums | Verified by `./x release-check v0.1.0`; rerun on the final release commit |
| Native artifact sessions and isolated uninstall | Passed on Linux and macOS in preparation PR #17 and infrastructure PR #18 CI |
| Typing on a real host | Maintainer reports checked; OS, agent and terminal versions were not recorded |
| Sleep and reconnect | Maintainer reports checked |
| Clipboard and image paste | Maintainer reports checked |
| Browser login | Still needs a real-host check |
| Uninstall on a real remote host | Still needs a check; isolated local uninstall is covered automatically |
| Release publication | Maintainer authorized v0.1.0 after release checks; browser login and real-host uninstall remain unverified |

The maintainer's reports are hands-on evidence, not an independently reproduced
OS/agent matrix. The maintainer authorized v0.1.0 with browser login and real-host
uninstall still unverified; those checks must not be represented as completed.
Record the OS, agent and terminal versions when those checks are performed. Use a disposable
account or machine for uninstall; keep credentials, hostnames and private paths
out of committed evidence. See the [manual checks](testing.md#manual-checks-before-a-release).

## Final release steps

1. Merge the preparation PR after `check (ubuntu-latest)`,
   `check (macos-latest)` and `release-check` pass. CI runs the versioned archive
   check natively on Linux and macOS.
2. Review the outstanding real-host checks above and the maintainer's explicit
   release decision. Confirm the final source commit and `v0.1.0` notes. Run `./x check` and
   `./x audit` and `./x release-check v0.1.0` from that commit.
3. With explicit approval to tag, create an annotated `v0.1.0` tag on the
   qualified commit reachable from `main`, then push that tag. Alternatively,
   dispatch the release workflow using the same tag as its workflow ref:
   `gh workflow run release.yml --ref v0.1.0 -f tag=v0.1.0`. A manual run from
   `main` is not allowed: the run's source identity must match the built tag
   for its provenance to be accurate.
4. The workflow reruns source and native artifact checks on Linux and macOS,
   builds and attests the archives and waits for the `release` environment approval. It
   verifies the selected tag, source commit and hashes, then creates a **draft**.
5. Review the draft's version, source commit, notes, four archives and
   `checksums.txt` and provenance. Download and verify a native archive. Publishing
   the draft is a separate, explicit maintainer action; it starts the install
   checks and protected Homebrew formula update described below.

No command in `./x` publishes, creates tags or changes repository settings.

## Published releases and formula updates

Aft uses `fschrhunt/aft` as its own Homebrew tap. The workflow attests archives
before creating the draft. Verify a downloaded archive with:

```sh
gh attestation verify ARCHIVE --repo fschrhunt/aft
```

The v0.1.0 release predates the repository transfer. Verify its provenance under the
original organization instead:

```sh
gh attestation verify ARCHIVE --owner arocomputer \
  --signer-workflow arocomputer/aft/.github/workflows/release.yml
```

Publishing the draft triggers checksum-verified native installation checks on
Linux and macOS. After those pass, the `formula` job generates `aft.rb` from the
published checksums and opens a release PR against `main`. Review and merge it
after required checks pass; automation never merges it. The job refuses tags
outside `main` and leaves the formula unchanged for older releases.

### Formula retries

To retry the formula for an already published release without rebuilding,
retagging or publishing anything, dispatch from `main`:

```sh
gh workflow run release.yml --ref main -f tag=v0.1.0 -F formula_only=true
```

This path verifies the native published downloads on Linux and macOS before
the formula job. An unchanged formula creates no branch or PR.
Each release uses its own branch; retries reuse only an open PR from the same
author with one unchanged automation commit that modifies only `aft.rb` and
matches the generated formula content and file mode. Human edits and closed PRs
require manual review and are never overwritten.

If branch creation succeeds but PR creation fails, retrying opens the PR only
when that branch passes the same content and automation ownership checks.

## What users receive

Archives are provided for Linux and macOS on amd64 and arm64. Each contains
`aft`, all four embedded servers, the license, README, brand assets and guides.
Every client and server reports the selected release version and the source
commit time used for server upgrade ordering. `release-notes.md` records the
source commit. SHA-256 checksums cover the four archives.

Archives are unsigned, and macOS binaries are not notarized. Explain this in
the [installation guide](../guides/start/install.md); never tell users to disable
system-wide protections. Cross-build success verifies packaging, not runtime
behavior on every CPU/OS combination.

## Local packaging

`./x release-check [vX.Y.Z]` uses a temporary directory and removes its archives.
Without a version it qualifies a development build; a version selects the exact
changelog section and verifies that version in the native binaries.

`./x dist v0.1.0` requires a clean committed checkout and writes archives,
`checksums.txt` and release notes to a new `dist/v0.1.0` directory. It refuses to
overwrite one. These local archives are for qualification; the release workflow
rebuilds the published candidate from the selected tag.

Download checksums from the trusted release and verify with
`sha256sum --check checksums.txt` or `shasum -a 256 --check checksums.txt` when
all four archives are present. For a single archive, use the filename-filtered
commands in the installation guide.
