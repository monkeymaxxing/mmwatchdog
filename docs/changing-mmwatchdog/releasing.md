# Release MMWatchdog

**Who acts:** a maintainer of MMWatchdog, or the maintainer's coding agent,
in a checkout of the MMWatchdog repository. Publishing needs the right to
push tags to the public repository `monkeymaxxing/mmwatchdog` on GitHub,
and to edit its releases.

**When to use this page:** the code for a new release is on the main
branch, its tests pass, and you want to publish the release archives that
users download.

**What this page does:** you choose the version number, run the checks
that only a release needs, push a tag, and GitHub builds and publishes the
archives. Then you write the part of the release notes that tells users
what they must change.

**Terms:**

- A **release archive** is `mmwatchdog-VERSION-linux-ARCH.tar.gz`, one for
  `amd64` (x86-64) and one for `arm64` (AArch64). Users unpack one archive
  as their MMX project, as [Install MMWatchdog on one node](../how-to/install-on-one-node.md)
  shows.
- `SHA256SUMS` lists the SHA-256 hash of each archive, so users can check
  their download.

## Choose the version number

MMWatchdog uses semantic versioning, starting at 0.1.0, as MMX does. The
version number tells users whether an update needs work from them:

| Release | Example | What it may contain |
|---|---|---|
| Patch | 0.1.1 after 0.1.0 | Fixes of faults and text changes, such as the documentation or a message. A user changes nothing. |
| Minor, while the version starts with `0.` | 0.2.0 after 0.1.1 | New commands, modules, and fields, and changes that existing users must act on, such as a renamed setting, a changed file format, or work during the upgrade. |
| Major, from 1.0.0 | 2.0.0 after 1.4.0 | Changes that need a user's edit. From 1.0.0, only a major release may contain them. |

Choose the smallest step that is honest. Gather small text fixes on the
main branch and release them together, unless one of them fixes something
that people would otherwise get wrong.

A change needs a user's edit when a configuration file no longer
validates or behaves differently, when a command, a flag, or a result
field is renamed or removed, when the new release cannot read a state
file, or when a hub cannot read the reports of nodes one minor release
older. [Compatibility between releases](design/README.md#compatibility-between-releases)
owns these rules, and the rule for a hub and its nodes.

Two rules of the version number come from the build:

- **A version with a hyphen is a prerelease,** such as `0.2.0-rc.1`.
  GitHub marks its release as a prerelease.
- **`0.1.0-dev` is never released.** The repository's
  `official_extra_modules/shipped_with_mmwatchdog_version` holds
  `0.1.0-dev`, the version of every build that is not a release, and the
  release workflow refuses that version.

`scripts/release.py` accepts a version of the form `0.1.0` or
`0.1.0-rc.1`, and refuses any other with `version must look like 0.1.0 or
0.1.0-rc.1`.

## Run the checks of a release

Some checks are too slow or need outside accounts, so they run only
before a release. Run each on the commit that you will tag:

- **Every test suite on every target distribution,** on AMD64, and the
  ARM64 suites on ARM64: see [Test in levels](testing.md#test-in-levels)
  and [Every target distribution](testing.md#every-target-distribution).
- **Static analysis and fuzzing:** see
  [Static analysis for a release](testing.md#static-analysis-for-a-release)
  and [Fuzz tests for a release](testing.md#fuzz-tests-for-a-release).
- **The real alert destinations:** Slack, Telegram, Discord, and Pushover,
  with a person who checks the channel and the phone. See
  [Real services before a release](testing.md#real-services-before-a-release).

The [release checklist](design/README.md#release-checklist) says what each
check must show.

## Build the archives on your machine

You can build the archives on your machine first, such as to look at what
a release holds. In a clean checkout:

```sh
python3 scripts/release.py 0.2.0
```

The script prints the directory where it wrote the files. Without
`--output-dir DIR`, the directory is `releases/0.2.0` in the testing
directory: `$MMWATCHDOG_TEST_ROOT`, or `~/.cache/mmwatchdog-testing`
when the variable is not set. The script waits while a test run holds the
lock of the testing directory, and says so.

The script publishes nothing and creates no Git tag. The script refuses a
checkout with uncommitted changes, so that each archive matches one
commit. To try the packaging with uncommitted changes, add
`--allow-dirty`. Such a build says `modified=true` in `mmwatchdog version`,
and `"modified": true` in its `manifest.json`.

The directory then holds:

| File | What it holds |
|---|---|
| `mmwatchdog-0.2.0-linux-amd64.tar.gz` and `mmwatchdog-0.2.0-linux-arm64.tar.gz` | The release archives. |
| `SHA256SUMS` | The SHA-256 hash of each archive. |
| `build.json` | The version, the Git revision, whether the checkout was modified, the Go version, and the hash of each archive. |

## What a release archive holds

Each archive has one directory, `mmwatchdog-VERSION-linux-ARCH/`, which is
the user's MMX project:

- **The program** `mmwatchdog`, built for Linux without cgo
  (`CGO_ENABLED=0`). `mmwatchdog version` prints the release's version and
  Git revision, which the build sets with linker flags.
- **The systemd unit** `mmwatchdog.service`.
- **The MMX project:** `mmx_project_settings.yml`, the playbooks
  `hub_and_standalone.yml` and `join_hub.yml`, the inventories
  `hub_and_standalone.yml` and `hub_members.yml`, the starter
  configuration in `files/`, and `mmwatchdog_external_modules/` for the
  project's own modules. All of them come from `examples/deploy/` in the
  repository.
- **The official extra modules** in `official_extra_modules/`. Their file
  `shipped_with_mmwatchdog_version` holds the release's version, and the
  program refuses official extra modules of another version.
- **The source:** `docs/`, `examples/`, `cmd/`, `internal/`, `scripts/`,
  `tests/`, `third_party/`, `go.mod`, `README.md`, and `LICENSE`.
- **The license notices** of Go and of each dependency, in `licenses/`.
- **`manifest.json`,** with the version, the revision, whether the
  checkout was modified, the system and processor, and the SHA-256 hash of
  every other file in the archive.

Each file in an archive belongs to `root`, with the time of the commit.

## Publish the release

Pushing a tag to the public repository on GitHub publishes the release.
The tag is `v` and the version. Tag the commit that passed the checks of
a release, and push the tag to the public repository, here the remote
`origin`:

```sh
git tag v0.2.0
git push origin v0.2.0
```

The push runs the workflow `.github/workflows/release.yml` on GitHub. The
workflow does these steps, in order, and stops at the first that fails:

1. **Checks the tag.** A tag without the `v` fails with `Release tag must
   start with v, such as v0.1.0.` The development version fails with
   `The development version 0.1.0-dev cannot be published as a release.`
2. **Installs Go** at the version of the `toolchain` line of `go.mod`, and
   confirms that version.
3. **Builds the archives** with `python3 scripts/release.py VERSION
   --output-dir DIR`, as on your machine.
4. **Checks the archives** against `SHA256SUMS` with `sha256sum --check`.
5. **Attests each archive's provenance** with GitHub's build provenance
   attestation, which records that this workflow built the archive.
6. **Creates the GitHub release** of the tag, titled `MMWatchdog VERSION`,
   with the two archives and `SHA256SUMS`, and notes that GitHub generates
   from the pull requests. A version with a hyphen becomes a prerelease.

**Check:** the release page lists the two archives and `SHA256SUMS`. The
files are at
`https://github.com/monkeymaxxing/mmwatchdog/releases/download/v0.2.0/`,
such as `mmwatchdog-0.2.0-linux-amd64.tar.gz`, where the install and
update pages download them.

## Write "Changes you must act on"

Users read the release notes before they update.
[Update MMWatchdog](../how-to/update-mmwatchdog.md), in step 6, tells them
to make each change that the notes list under "Changes you must act on".
The notes that GitHub generates do not list those changes, so you write
the section by hand.

1. **Open the new release on GitHub,** and edit its notes.
2. **Add the section "Changes you must act on" at the top,** above the
   generated notes. List each change that a project or a node needs for
   this release, with the edit, such as a renamed setting or a new
   variable that every inventory must declare. For example, a release that
   renamed a setting would say:

   ```markdown
   ## Changes you must act on

   - The setting `remind_after` is now `remind_every`. In each
     `alert_destinations.yml`, rename `remind_after:` to `remind_every:`.
   ```

   Write "none" when users need to change nothing, as for every patch
   release.
3. **Save the notes before you announce the release.**

**Check:** the release page starts with "Changes you must act on", and
each change in it says what to edit.
