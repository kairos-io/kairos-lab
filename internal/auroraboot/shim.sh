#!/bin/sh
# kairos-lab-managed: auroraboot shim
#
# Runs the pinned AuroraBoot image in a container. `kairos-lab setup` writes
# this file and `kairos-lab cleanup` removes it; setup rewrites it on every
# run, so local edits do not last.
#
# The current directory is mounted at its own absolute path and used as the
# working directory, so paths in the arguments mean the same thing inside the
# container as on the host, and the arguments are passed through unchanged.

runtime='@@RUNTIME@@'
image='@@IMAGE@@'
os='@@OS@@'

nl='
'

die() {
	printf 'auroraboot: %s\n' "$*" >&2
	exit 2
}

# is_reserved reports whether a path must never be mounted into the container:
# it would shadow something the image itself needs, or it cannot be written as
# a -v argument.
is_reserved() {
	case $1 in
	*:* | *,* | *"$nl"*) return 0 ;;
	/ | /tmp) return 0 ;;
	/bin | /bin/* | /boot | /boot/* | /dev | /dev/* | /etc | /etc/*) return 0 ;;
	/lib | /lib/* | /lib64 | /lib64/* | /proc | /proc/* | /run | /run/*) return 0 ;;
	/sbin | /sbin/* | /sys | /sys/* | /usr | /usr/* | /var/run | /var/run/*) return 0 ;;
	/amd | /amd/* | /arm | /arm/* | /riscv64 | /riscv64/*) return 0 ;;
	esac
	return 1
}

# find_subcommand prints the AuroraBoot subcommand named in the arguments, or
# nothing when there is none. It is the first word that does not start with a
# dash, once the values of the two global flags that take one are skipped.
find_subcommand() {
	skip=0
	for arg in "$@"; do
		if [ "$skip" = 1 ]; then
			skip=0
			continue
		fi
		case $arg in
		--set | --cloud-config) skip=1 ;;
		-*) ;;
		build-iso | bi | build-uki | bu | genkey | gk | sysext | confext | netboot | nb)
			printf '%s' "$arg"
			return 0
			;;
		uki-pxe | start-pixie | sp | web | w | redfish | unpack | pull)
			printf '%s' "$arg"
			return 0
			;;
		*) return 0 ;;
		esac
	done
}

# warn_build_iso_without_output exists only because AuroraBoot writes to a
# directory inside the container when build-iso gets no --output, which loses
# the ISO. Delete this function and its call once the pinned image writes to
# the current directory instead.
warn_build_iso_without_output() {
	case $subcommand in
	build-iso | bi) ;;
	*) return 0 ;;
	esac
	for arg in "$@"; do
		case $arg in
		--output | --output=* | -o | -o=*) return 0 ;;
		esac
	done
	printf '%s\n' 'auroraboot: warning: no --output given, so the ISO is written inside the container and lost; pass --output <dir>' >&2
}

# find_socket prints the host path of the container runtime's socket, or
# nothing when it cannot be found. Registry images work without it.
find_socket() {
	case $runtime in
	docker)
		if [ "$os" = darwin ]; then
			printf '%s\n' /var/run/docker.sock
			return 0
		fi
		case ${DOCKER_HOST:-} in
		unix://*)
			printf '%s\n' "${DOCKER_HOST#unix://}"
			return 0
			;;
		esac
		host=$("$runtime" context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null) || return 0
		case $host in
		unix://*) printf '%s\n' "${host#unix://}" ;;
		esac
		;;
	podman)
		[ "$os" = linux ] || return 0
		"$runtime" info --format '{{.Host.RemoteSocket.Path}}' 2>/dev/null || true
		;;
	esac
}

cwd=$(pwd -P) || die 'cannot resolve the current directory'
if is_reserved "$cwd"; then
	die "refusing to run in $cwd: it cannot be mounted into the container"
fi

subcommand=$(find_subcommand "$@")
warn_build_iso_without_output "$@"

# The options are built by prepending to the arguments, last option first, so
# the arguments themselves reach the container untouched and word for word.
set -- "$image" "$@"
set -- -w "$cwd" "$@"
set -- -v "$cwd:$cwd" "$@"

sock=$(find_socket)
if [ -n "$sock" ]; then
	if [ "$os" = darwin ] || [ -S "$sock" ]; then
		set -- -v "$sock:/var/run/docker.sock" "$@"
	fi
fi

if [ "$os" = linux ]; then
	set -- --net host --security-opt label=disable "$@"
fi

if [ -t 0 ] && [ -t 1 ]; then
	set -- -t "$@"
fi

exec "$runtime" run --rm -i "$@"
