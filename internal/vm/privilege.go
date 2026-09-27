package vm

import (
	"fmt"
	"strings"
)

// RequireNetworkPrivilege fails when the host cannot give kairos-lab the
// privilege the chosen network mode needs, and is a no-op everywhere that
// question does not arise.
//
// Today that is macOS and only macOS. Both vmnet modes there -- shared
// (vmnet-shared) and bridged (vmnet-bridged) -- require root: Apple gates
// unprivileged vmnet access behind the com.apple.vm.networking entitlement,
// which is granted to virtualization vendors and not to arbitrary binaries,
// and QEMU's own request to work without it is still open. A Homebrew
// qemu-system-aarch64 therefore has exactly one way onto a vmnet interface,
// which is to be launched under sudo -- so this is a fact about the platform
// and not a policy this tool chose.
//
// Linux needs nothing here. Its bridge and tap are built by nmcli and ip
// through the sudo helper in network_linux.go, one command at a time, and the
// tap is handed to the invoking user's uid precisely so that QEMU itself runs
// unprivileged.
//
// The check is split in two: the platform file gathers facts and nothing
// else, and checkDarwinPrivilege below turns those facts into an answer. That
// is what makes the decision -- including the wording of the refusal, which
// carries a requirement of its own -- testable on both CI legs rather than
// only on a macOS runner with the right group membership.
func RequireNetworkPrivilege(mode string) error {
	return checkNetworkPrivilege(mode)
}

// darwinRootModes are the network modes that cannot work without root on
// macOS. Anything else -- "user", and any unknown or empty value -- needs no
// privilege, and that is deliberately a positive list rather than "everything
// except user": buildMacOS falls back to user networking for every mode it
// does not recognise, so refusing to start on an unrecognised mode would
// refuse a run that was about to use SLIRP and need nothing at all.
var darwinRootModes = []string{"shared", "bridged"}

func darwinModeNeedsRoot(mode string) bool {
	for _, m := range darwinRootModes {
		if mode == m {
			return true
		}
	}
	return false
}

// darwinAdminGroups are the groups whose members sudo grants on a stock
// macOS. admin is the one the Settings checkbox "Allow user to administer
// this computer" manages; wheel is the BSD group underneath it, which
// /etc/sudoers also grants and which a locally managed account can be in
// without being in admin.
var darwinAdminGroups = []string{"admin", "wheel"}

// checkDarwinPrivilege is the whole decision, as a pure function of four
// facts: the mode asked for, the effective uid, whether a sudo binary exists,
// and the groups the current user belongs to.
//
// It is satisfied three ways: the mode needs no privilege; we are already
// root (the user ran the whole tool under sudo); or sudo exists AND the user
// is in a group it grants, which means the escalation the run will perform
// can succeed even though we are not root yet.
//
// Group membership is checked rather than assumed for a reason: without it,
// the failure moves to the middle of a start, after the disk has been created
// and QEMU has been assembled, where it arrives as an opaque sudo password
// prompt that refuses three times and then a vmnet error from QEMU. The point
// of doing it here is to say the true thing before anything has been built.
func checkDarwinPrivilege(mode string, euid int, hasSudo bool, groups []string) error {
	if !darwinModeNeedsRoot(mode) {
		return nil
	}
	if euid == 0 {
		return nil
	}
	if hasSudo && hasAdminGroup(groups) {
		return nil
	}

	reason := "this process is not running as root and the current user is in neither the admin nor the wheel group, so sudo will not grant it either"
	if !hasSudo {
		reason = "this process is not running as root and there is no sudo binary on PATH to become it"
	}
	// The fallback sentence is a requirement and not decoration. A user who
	// cannot get admin rights on this machine -- a managed laptop, a shared
	// CI mac -- can still run one VM, and needs to know the limit of that
	// before building a workshop around it rather than after.
	return fmt.Errorf(
		"--network %s needs root on macOS: %s. Apple restricts the com.apple.vm.networking entitlement to virtualization vendors, so a Homebrew QEMU can reach a vmnet interface only when it is launched under sudo. Re-run with sudo, or use --network user instead -- it needs no privileges, but it supports a single VM and no cluster: the guest sits behind QEMU's user-mode NAT, reachable only on the ports forwarded to localhost, with no address on your network, so a second VM cannot reach it and the two cannot form a cluster",
		mode, reason,
	)
}

func hasAdminGroup(groups []string) bool {
	for _, g := range groups {
		for _, admin := range darwinAdminGroups {
			if g == admin {
				return true
			}
		}
	}
	return false
}

// parseGroupNames splits the single whitespace-separated line `id -Gn` prints
// into group names. It lives here rather than in privilege_darwin.go so that
// the split is exercised on both CI legs; the darwin file is left with
// nothing but fact gathering.
func parseGroupNames(out string) []string {
	return strings.Fields(out)
}
