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
# Any other host path named in the arguments is mounted at its own path too.
#
# Known limits: values given to --set are not scanned for paths, and
# AURORABOOT_* environment variables are not passed into the container.

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
# a -v argument. Callers check a path both as written and after symlinks are
# resolved: on macOS /etc, /tmp and /var are links into /private, so the
# resolved spelling of a system directory is not the one people type.
is_reserved() {
	r=$1
	while :; do
		case $r in
		//*) r=${r#/} ;;
		*) break ;;
		esac
	done
	case $r in
	*:* | *,* | *"$nl"*) return 0 ;;
	/ | /tmp | /var) return 0 ;;
	/bin | /bin/* | /boot | /boot/* | /dev | /dev/* | /etc | /etc/*) return 0 ;;
	/lib | /lib/* | /lib64 | /lib64/* | /proc | /proc/* | /run | /run/*) return 0 ;;
	/sbin | /sbin/* | /sys | /sys/* | /usr | /usr/* | /var/run | /var/run/*) return 0 ;;
	/amd | /amd/* | /arm | /arm/* | /riscv64 | /riscv64/*) return 0 ;;
	/private | /private/var | /private/etc | /private/etc/* | /private/tmp | /private/var/run | /private/var/run/*) return 0 ;;
	/System | /System/* | /Library | /Library/* | /Applications | /Applications/*) return 0 ;;
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

# flag_class prints how the flag named in $1 treats the value it takes: INPUT
# for a host path AuroraBoot reads, OUTPUT for one it writes, SKIP for a value
# that is not a path. It prints nothing for a flag it does not know.
flag_class() {
	case $1 in
	--set)
		printf '%s' SKIP
		return 0
		;;
	--cloud-config)
		printf '%s' INPUT
		return 0
		;;
	esac
	case $subcommand in
	build-iso | bi)
		case $1 in
		--output | -o) printf '%s' OUTPUT ;;
		-c | --overlay-rootfs | --overlay-iso | --extensions-catalog) printf '%s' INPUT ;;
		esac
		;;
	build-uki | bu)
		case $1 in
		--output-dir | -d) printf '%s' OUTPUT ;;
		--overlay-rootfs | -o | --overlay-iso | -i | --public-keys) printf '%s' INPUT ;;
		--tpm-pcr-private-key | --sb-key | --sb-cert | --splash | --extensions-catalog) printf '%s' INPUT ;;
		esac
		;;
	genkey | gk)
		case $1 in
		--output | -o) printf '%s' OUTPUT ;;
		--custom-cert-dir) printf '%s' INPUT ;;
		esac
		;;
	sysext | confext)
		case $1 in
		--output) printf '%s' OUTPUT ;;
		--private-key | --certificate) printf '%s' INPUT ;;
		esac
		;;
	web | w)
		case $1 in
		--data-dir | --artifacts-dir) printf '%s' OUTPUT ;;
		--db | --tls-cert | --tls-key | --redfish-serve-tls-cert) printf '%s' INPUT ;;
		--redfish-serve-tls-key | --redfish-quirks-dir | --kubeconfig) printf '%s' INPUT ;;
		esac
		;;
	redfish)
		case $1 in
		--password-file | --quirks-dir | --serve-tls-cert | --serve-tls-key) printf '%s' INPUT ;;
		esac
		;;
	unpack | pull)
		case $1 in
		--arch | --loglevel | -l) printf '%s' SKIP ;;
		esac
		;;
	uki-pxe)
		case $1 in
		--loglevel | -l) printf '%s' SKIP ;;
		esac
		;;
	esac
}

# is_under reports whether path $1 is the directory $2 or inside it.
is_under() {
	[ "$1" = "$2" ] && return 0
	case $1 in
	"$2"/*) return 0 ;;
	esac
	return 1
}

# resolve_existing prints the physical path of the existing file or directory
# $1, following symlinks all the way, so a link that points outside the
# current directory is mounted through its target.
resolve_existing() {
	f=$1
	n=0
	while [ -L "$f" ] && [ "$n" -lt 20 ]; do
		t=$(readlink "$f") || return 1
		case $t in
		/*) f=$t ;;
		*)
			f=$(dirname "$f")
			if [ "$f" = / ]; then
				f=/$t
			else
				f=$f/$t
			fi
			;;
		esac
		n=$((n + 1))
	done
	if [ -d "$f" ]; then
		(cd -P "$f" 2>/dev/null && pwd -P)
		return
	fi
	[ -e "$f" ] || return 1
	d=$(cd -P "$(dirname "$f")" 2>/dev/null && pwd -P) || return 1
	if [ "$d" = / ]; then
		printf '/%s\n' "$(basename "$f")"
	else
		printf '%s/%s\n' "$d" "$(basename "$f")"
	fi
}

# resolve_missing prints the physical path a not yet existing directory $1
# would have: its closest existing ancestor, resolved, plus the missing tail.
resolve_missing() {
	q=$1
	rest=
	while [ ! -e "$q" ] && [ ! -L "$q" ] && [ "$q" != / ]; do
		b=$(basename "$q")
		case $b in
		. | ..) return 1 ;;
		esac
		rest=/$b$rest
		q=$(dirname "$q")
	done
	base=$(resolve_existing "$q") || return 1
	[ "$base" = / ] && base=
	printf '%s%s\n' "$base" "$rest"
}

# add_mount records host path $1 to be mounted at itself, unless it is already
# covered by a directory on the list.
add_mount() {
	while IFS= read -r m; do
		[ -n "$m" ] || continue
		if is_under "$1" "$m"; then
			return 0
		fi
	done <<EOF
$mounts
EOF
	mounts=$mounts$1$nl
}

# add_path handles one host path named in the arguments. $1 is INPUT or
# OUTPUT, $2 the value as written, $3 is "strict" for a path given to a flag
# that is known to take one and "lenient" for any other token. A strict path
# that cannot be mounted stops the run; a lenient one is left alone, since it
# may not be a path at all.
add_path() {
	v=$2
	case $v in
	'' | - | *://* | pkcs11:*) return 0 ;;
	esac
	case $v in
	/*) a=$v ;;
	*) a=$cwd/$v ;;
	esac
	# Never let a doubled leading slash through into a mount argument.
	while :; do
		case $a in
		//*) a=${a#/} ;;
		*) break ;;
		esac
	done
	created=0
	if [ -e "$a" ] || [ -L "$a" ]; then
		p=$(resolve_existing "$a") || return 0
	elif [ "$1" = OUTPUT ]; then
		p=$(resolve_missing "$a") || return 0
		created=1
	else
		return 0
	fi
	if is_under "$p" "$cwd"; then
		return 0
	fi
	if is_reserved "$a" || is_reserved "$p"; then
		if [ "$3" = strict ]; then
			die "refusing to mount $a (resolves to $p): it cannot be shared with the container"
		fi
		return 0
	fi
	if [ "$created" = 1 ]; then
		mkdir -p "$p" || die "cannot create $p"
	fi
	add_mount "$p"
}

# add_token handles a token that is not known to be a path: it may name a
# host path, an image, or neither. A dir:, file: or ocifile: source names a
# host path; docker: and oci: sources and bare image references do not.
add_token() {
	x=$1
	case $x in
	docker:* | oci:*) return 0 ;;
	dir:* | file:* | ocifile:*)
		x=${x#*:}
		case $x in
		//*) x=${x#//} ;;
		esac
		;;
	esac
	add_path INPUT "$x" lenient
}

# scan_args walks the arguments once and records every host path they name.
scan_args() {
	pending=
	seen=0
	pos=0
	for arg in "$@"; do
		if [ -n "$pending" ]; then
			case $pending in
			SKIP) ;;
			*) add_path "$pending" "$arg" strict ;;
			esac
			pending=
			continue
		fi
		case $arg in
		--*=* | -?=*)
			cls=$(flag_class "${arg%%=*}")
			case $cls in
			OUTPUT | INPUT) add_path "$cls" "${arg#*=}" strict ;;
			SKIP) ;;
			*) add_token "${arg#*=}" ;;
			esac
			;;
		-*)
			pending=$(flag_class "$arg")
			;;
		*)
			if [ "$seen" = 0 ]; then
				seen=1
				[ -n "$subcommand" ] && continue
			fi
			pos=$((pos + 1))
			case $subcommand:$pos in
			netboot:2 | nb:2 | unpack:2 | pull:2) add_path OUTPUT "$arg" strict ;;
			*) add_token "$arg" ;;
			esac
			;;
		esac
	done
}

cwd=$(pwd -P) || die 'cannot resolve the current directory'
logical=$(pwd)
if is_reserved "$cwd" || is_reserved "$logical"; then
	die "refusing to run in $logical ($cwd): it cannot be mounted into the container"
fi

subcommand=$(find_subcommand "$@")
warn_build_iso_without_output "$@"

mounts=
scan_args "$@"

# The options are built by prepending to the arguments, last option first, so
# the arguments themselves reach the container untouched and word for word.
set -- "$image" "$@"
set -- -w "$cwd" "$@"
set -- -v "$cwd:$cwd" "$@"

while IFS= read -r m; do
	if [ -n "$m" ]; then
		set -- -v "$m:$m" "$@"
	fi
done <<EOF
$mounts
EOF

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
