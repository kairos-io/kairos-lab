#!/bin/sh
# kairos-lab installer.
#
# Downloads the latest kairos-lab release for this Linux machine, verifies it
# against the release's checksums.txt, and installs the binary onto PATH.
#
# Usage:
#   curl -sSL https://raw.githubusercontent.com/kairos-io/kairos-lab/main/install.sh | sh
#   wget -qO- https://raw.githubusercontent.com/kairos-io/kairos-lab/main/install.sh | sh
#
# Written in POSIX sh so it runs on whatever /bin/sh a fresh machine ships,
# without assuming bash, jq, or the gh CLI are installed.
#
# Everything the script does lives inside functions, and the only top-level
# statement is the "main" call at the very end of the file. That is
# deliberate: when this script is run as `curl ... | sh`, the shell reads and
# executes it as the bytes arrive. If the download is cut short partway
# through a plain top-to-bottom script, the shell can end up executing a
# truncated command instead of failing outright. Keeping every action inside
# a function and calling main only once, on the last line, means a partial
# download almost always fails to parse at all (a function body left
# unclosed) or silently does nothing (the final "main" call itself is
# missing), rather than running half of the install.

set -eu

REPO="kairos-io/kairos-lab"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"
ASSUME_YES=0

log() {
	printf '%s\n' "$*"
}

err() {
	printf 'kairos-lab install: %s\n' "$*" >&2
}

die() {
	err "$*"
	exit 1
}

usage() {
	cat <<EOF
Usage: install.sh [-y|--yes] [-h|--help]

Downloads and installs the latest kairos-lab release for this machine.

  -y, --yes    skip the confirmation prompt and proceed (for scripted use)
  -h, --help   show this help and exit

kairos-lab: https://github.com/kairos-io/kairos-lab
EOF
}

need_cmd() {
	command -v "$1" >/dev/null 2>&1
}

parse_args() {
	while [ $# -gt 0 ]; do
		case "$1" in
		-y | --yes)
			ASSUME_YES=1
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			die "unknown argument: $1 (see --help)"
			;;
		esac
		shift
	done
}

# Sets $os_kind and $arch, or exits with a clear error.
detect_platform() {
	os_kind=$(uname -s)
	case "$os_kind" in
	Linux) ;;
	*)
		die "this script only installs kairos-lab on Linux (detected: ${os_kind}). See https://github.com/${REPO}#install for other platforms."
		;;
	esac

	machine=$(uname -m)
	case "$machine" in
	x86_64 | amd64)
		arch=amd64
		;;
	aarch64 | arm64)
		arch=arm64
		;;
	*)
		die "unsupported architecture: ${machine} (kairos-lab publishes linux amd64 and arm64 builds only)"
		;;
	esac
}

# Prints $1 (a URL) to stdout, or dies with a clear error.
fetch() {
	url=$1
	if need_cmd curl; then
		curl --fail --silent --show-error --location "$url"
	elif need_cmd wget; then
		wget --quiet -O - "$url"
	else
		die "need curl or wget to download files, and neither is installed"
	fi
}

# Downloads $1 (a URL) to the file at $2.
fetch_to_file() {
	url=$1
	dest=$2
	if need_cmd curl; then
		curl --fail --silent --show-error --location --output "$dest" "$url"
	elif need_cmd wget; then
		wget --quiet -O "$dest" "$url"
	else
		die "need curl or wget to download files, and neither is installed"
	fi
}

# Prompts with $1 and returns success only on an affirmative answer.
# Assumes yes without prompting when -y/--yes was given.
confirm() {
	prompt=$1
	if [ "$ASSUME_YES" -eq 1 ]; then
		return 0
	fi

	# `curl ... | sh` leaves stdin attached to the script itself, not a
	# terminal, so prefer /dev/tty when one is actually usable and fall
	# back to stdin (e.g. when the script was downloaded and run
	# directly, or there is no controlling terminal at all).
	if (: >/dev/tty) 2>/dev/null; then
		printf '%s [y/N] ' "$prompt" >/dev/tty
		if ! read -r reply </dev/tty; then
			reply=""
		fi
	else
		printf '%s [y/N] ' "$prompt"
		if ! read -r reply; then
			reply=""
		fi
	fi

	case "$reply" in
	y | Y | yes | Yes | YES)
		return 0
		;;
	*)
		return 1
		;;
	esac
}

# Sets $install_dir and $use_sudo. Makes no changes to the filesystem.
choose_install_dir() {
	use_sudo=0
	if [ -w /usr/local/bin ] 2>/dev/null; then
		install_dir=/usr/local/bin
	elif need_cmd sudo; then
		install_dir=/usr/local/bin
		use_sudo=1
	else
		install_dir="${HOME}/.local/bin"
	fi
}

path_has() {
	case ":${PATH}:" in
	*":$1:"*)
		return 0
		;;
	*)
		return 1
		;;
	esac
}

# Copies $1 to $2 and makes it executable, using sudo when $use_sudo is 1.
install_binary() {
	src=$1
	dest=$2
	dest_dir=$(dirname "$dest")

	if [ "$use_sudo" -eq 1 ]; then
		sudo mkdir -p "$dest_dir" || die "failed to create ${dest_dir}"
		sudo cp "$src" "$dest" || die "failed to copy kairos-lab to ${dest}"
		sudo chmod 0755 "$dest" || die "failed to make ${dest} executable"
	else
		mkdir -p "$dest_dir" || die "failed to create ${dest_dir}"
		cp "$src" "$dest" || die "failed to copy kairos-lab to ${dest}"
		chmod 0755 "$dest" || die "failed to make ${dest} executable"
	fi
}

make_workdir() {
	if need_cmd mktemp; then
		workdir=$(mktemp -d) || die "failed to create a temporary working directory"
	else
		workdir="${TMPDIR:-/tmp}/kairos-lab-install.$$"
		mkdir -p "$workdir" || die "failed to create a temporary working directory"
	fi
}

main() {
	parse_args "$@"
	detect_platform

	need_cmd sha256sum || die "sha256sum is required to verify the download, but is not installed"
	need_cmd tar || die "tar is required to extract the download, but is not installed"

	log "Looking up the latest kairos-lab release..."
	release_json=$(fetch "$API_URL") || die "failed to query the GitHub releases API: ${API_URL}"

	tag=$(printf '%s' "$release_json" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
	[ -n "$tag" ] || die "could not determine the latest kairos-lab version from the GitHub API response"
	version=${tag#v}

	archive="kairos-lab_${version}_linux_${arch}.tar.gz"
	archive_url="https://github.com/${REPO}/releases/download/${tag}/${archive}"
	checksums_url="https://github.com/${REPO}/releases/download/${tag}/checksums.txt"

	choose_install_dir

	log ""
	log "This will install kairos-lab as follows:"
	log "  version      : ${tag}"
	log "  platform     : linux/${arch}"
	log "  archive      : ${archive_url}"
	log "  checksums    : ${checksums_url}"
	log "  install path : ${install_dir}/kairos-lab"
	if [ "$use_sudo" -eq 1 ]; then
		log "  note         : ${install_dir} is not writable by your user, so this script will run 'sudo' to install there"
	fi
	log ""

	if ! confirm "Proceed?"; then
		log "Aborted. Nothing was changed."
		exit 0
	fi

	make_workdir
	trap 'rm -rf "$workdir"' EXIT INT TERM

	log "Downloading ${archive}..."
	fetch_to_file "$archive_url" "${workdir}/${archive}" || die "failed to download ${archive_url}"
	fetch_to_file "$checksums_url" "${workdir}/checksums.txt" || die "failed to download ${checksums_url}"

	log "Verifying checksum..."
	checksum_line=$(grep -F "  ${archive}" "${workdir}/checksums.txt" || true)
	[ -n "$checksum_line" ] || die "no checksum entry for ${archive} in ${checksums_url}"
	(cd "$workdir" && printf '%s\n' "$checksum_line" | sha256sum -c - >/dev/null) ||
		die "checksum verification failed for ${archive}; the download may be corrupt or tampered with"

	log "Extracting..."
	tar -xzf "${workdir}/${archive}" -C "$workdir" kairos-lab || die "failed to extract kairos-lab from ${archive}"
	chmod +x "${workdir}/kairos-lab" || die "failed to make the extracted binary executable"

	log "Installing to ${install_dir}..."
	install_binary "${workdir}/kairos-lab" "${install_dir}/kairos-lab"

	log ""
	log "kairos-lab ${tag} installed to ${install_dir}/kairos-lab"

	if ! path_has "$install_dir"; then
		log ""
		log "warning: ${install_dir} is not on your PATH."
		log "Add it in your shell profile, for example:"
		log "  export PATH=\"${install_dir}:\$PATH\""
	fi
}

main "$@"
