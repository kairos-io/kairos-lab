//go:build darwin

package vm

import (
	"os"
	"os/exec"
)

// checkNetworkPrivilege gathers the four facts checkDarwinPrivilege decides
// on and does nothing else. Every branch of the decision lives in the pure
// function, which is what lets the refusal and its wording be tested on a
// host that is not a mac and by a user whose group membership is whatever it
// happens to be.
//
// os.Geteuid and not os.Getuid: under sudo the real uid is still the
// invoking user's, and the effective one is what actually decides whether
// this process may open /dev/vmnet.
func checkNetworkPrivilege(mode string) error {
	_, sudoErr := exec.LookPath("sudo")
	return checkDarwinPrivilege(mode, os.Geteuid(), sudoErr == nil, currentUserGroups())
}

// currentUserGroups returns the group names the current user belongs to, or
// nothing when they cannot be determined.
//
// `id -Gn` rather than os/user: it is the answer the system itself gives,
// including the groups granted by a directory service, which a purely local
// lookup would miss on a machine bound to one. An error is not reported --
// an unknown group list simply means no group can be matched, which is the
// same conservative answer as being in none.
func currentUserGroups() []string {
	out, err := exec.Command("id", "-Gn").Output()
	if err != nil {
		return nil
	}
	return parseGroupNames(string(out))
}
