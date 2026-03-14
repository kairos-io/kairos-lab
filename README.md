# kairos-lab

`kairos-lab` is a small Go CLI for a first-time local Kairos experience in workshops and quick testing.

It helps you:

- prepare your machine (`setup`)
- boot a Kairos ISO in a local VM (`start`)
- inspect state (`status`)
- clean VM artifacts (`reset`)
- clean everything created by the tool (`cleanup`)

This project is intentionally an MVP for local experimentation, not production deployment.

## Supported Platforms

- Linux (primary workshop target)
- macOS Apple Silicon (manual testing path)
- Windows: not supported

## Install / Build

```bash
go build -o kairos-lab ./cmd/kairos-lab
```

## Quick Start

### 1) Setup dependencies

```bash
./kairos-lab setup
```

The command detects your package manager and checks required tools (`qemu`, and Linux bridge tools).
If anything is missing, it asks before installing.

### 2) Start a VM from local ISO

```bash
./kairos-lab start --iso /path/to/kairos.iso
```

### 3) Or start from URL

```bash
./kairos-lab start --url https://example.org/kairos.iso
```

Default disk size is `60G`.

## Commands

### `setup`

- Detects OS/architecture and package manager
- Checks dependencies
- Installs missing dependencies only with confirmation
- Records dependency provenance:
  - pre-existing dependencies
  - dependencies installed by `kairos-lab`

### `start`

- Accepts exactly one source: `--iso` or `--url`
- Creates/uses managed disk image under cache directory
- Boots QEMU with workshop-oriented defaults
- Default network mode: `bridged`
- Runs QEMU attached to your terminal so you can interact with the boot console
- Stores runtime metadata (pid, args, disk/iso paths, log path)

Useful flags:

- `--network bridged|user` (default: `bridged`)
- `--disk-size 60G`
- `--memory 4096` / `--cpus 2`
- `--yes` (auto-confirm prompts)

### `stop`

- Stops the tracked VM process if running

### `status`

Shows:

- platform and package manager
- dependencies present and provenance
- managed files/directories
- ISO source/path
- VM status and pid
- network mode/resources

### `reset`

- Removes VM artifacts created by tool:
  - disk image
  - downloaded ISO (if tool-managed)
  - runtime metadata files
- Keeps setup/dependency tracking

### `cleanup`

- Removes everything created by `kairos-lab`
- Removes only dependencies installed by `kairos-lab`
- Never removes dependencies that existed before setup
- On Linux, removes bridged network resources only if created by `kairos-lab`

## Safety Model

Cleanup is conservative and provenance-based.

- The state file tracks what was pre-existing vs installed by the tool.
- Managed file/dir paths are tracked explicitly.
- Destructive operations only target tracked and safe paths.
- If safe cleanup cannot be guaranteed, command fails with an explanation.

## State and Artifact Paths

By default:

- state: `$XDG_CONFIG_HOME/kairos-lab/state.json`
- artifacts: `$XDG_CACHE_HOME/kairos-lab/`

You can override for testing:

- `KAIROS_LAB_CONFIG_DIR`
- `KAIROS_LAB_CACHE_DIR`

## CI

- Unit tests + build on Linux/macOS: `.github/workflows/ci.yml`
- Linux E2E (manual, kvm runner): `.github/workflows/e2e.yml`

E2E tests are tag-gated and not run by default.
