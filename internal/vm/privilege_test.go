// Tests for the macOS network-privilege check.
//
// Untagged on purpose, like the decision it covers: checkDarwinPrivilege is a
// pure function of four facts, so every branch of it -- including the one a
// macOS CI runner could never reach, because the runner's user IS an
// administrator -- is exercised on both legs. The platform file holds only
// the gathering of those facts.
package vm

import (
	"runtime"
	"strings"
	"testing"
)

// Group lists as `id -Gn` prints them on a stock macOS account.
var (
	adminGroups   = []string{"staff", "everyone", "localaccounts", "_appserverusr", "admin", "_lpadmin"}
	wheelGroups   = []string{"staff", "everyone", "localaccounts", "wheel"}
	neitherGroups = []string{"staff", "everyone", "localaccounts", "_developer"}
)

func TestCheckDarwinPrivilege(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		euid    int
		hasSudo bool
		groups  []string
		wantErr bool
	}{
		{"root running bridged", "bridged", 0, true, neitherGroups, false},
		{"root running shared, without even a sudo binary", "shared", 0, false, nil, false},
		{"non-root administrator, bridged", "bridged", 501, true, adminGroups, false},
		{"non-root administrator, shared", "shared", 501, true, adminGroups, false},
		{"non-root member of wheel", "shared", 501, true, wheelGroups, false},
		{"non-root in neither group", "bridged", 501, true, neitherGroups, true},
		{"non-root with no groups at all", "shared", 501, true, nil, true},
		{"administrator but no sudo binary on PATH", "bridged", 501, false, adminGroups, true},
		{"neither group and no sudo binary", "shared", 501, false, neitherGroups, true},
		// buildMacOS falls back to user networking for any mode it does not
		// recognise, so an unrecognised mode needs no privilege either.
		// Refusing here would refuse a run that was about to use SLIRP.
		{"unset mode needs nothing", "", 501, false, neitherGroups, false},
		{"unknown mode needs nothing", "quantum", 501, false, neitherGroups, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDarwinPrivilege(tc.mode, tc.euid, tc.hasSudo, tc.groups)
			if tc.wantErr && err == nil {
				t.Fatalf("checkDarwinPrivilege(%q, %d, %v, %v) = nil, want an error", tc.mode, tc.euid, tc.hasSudo, tc.groups)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkDarwinPrivilege(%q, %d, %v, %v) = %v, want nil", tc.mode, tc.euid, tc.hasSudo, tc.groups, err)
			}
		})
	}
}

// user mode needs nothing from anybody, in every state the host can be in.
// It is the fallback the refusal below points at, so it must never be the
// thing that is refused.
func TestCheckDarwinPrivilegeAlwaysAllowsUserMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		euid    int
		hasSudo bool
		groups  []string
	}{
		{"root", 0, true, adminGroups},
		{"non-root administrator", 501, true, adminGroups},
		{"non-root member of wheel", 501, true, wheelGroups},
		{"non-root in neither group", 501, true, neitherGroups},
		{"no sudo binary", 501, false, neitherGroups},
		{"no groups at all", 501, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkDarwinPrivilege("user", tc.euid, tc.hasSudo, tc.groups); err != nil {
				t.Fatalf("checkDarwinPrivilege(\"user\", ...) = %v, want nil", err)
			}
		})
	}
}

// The wording is the requirement, not decoration: a user who cannot get
// admin rights on this machine can still run one VM, and has to learn the
// limit of that before building a workshop around it.
func TestDarwinPrivilegeErrorNamesTheFallbackAndItsLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hasSudo    bool
		groups     []string
		wantReason string
	}{
		{"not an administrator", true, neitherGroups, "neither the admin nor the wheel group"},
		{"no sudo binary", false, adminGroups, "no sudo binary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDarwinPrivilege("bridged", 501, tc.hasSudo, tc.groups)
			if err == nil {
				t.Fatal("checkDarwinPrivilege = nil, want an error")
			}
			msg := err.Error()
			for _, want := range []string{
				// The mode the user actually asked for.
				"bridged",
				// The degraded fallback, spelled as it is typed.
				"--network user",
				// And what that fallback costs, said plainly.
				"single VM",
				"no cluster",
				// Why root is needed at all, so the refusal is not read as
				// this tool being precious about permissions.
				"vmnet",
				tc.wantReason,
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("error message does not mention %q:\n%s", want, msg)
				}
			}
		})
	}
}

func TestParseGroupNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want []string
	}{
		{"one line of groups", "staff admin everyone\n", []string{"staff", "admin", "everyone"}},
		{"trailing and repeated whitespace", "  staff   admin  \n", []string{"staff", "admin"}},
		{"a single group", "staff", []string{"staff"}},
		{"empty output", "", nil},
		{"whitespace only", " \n\t ", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseGroupNames(tc.out)
			if len(got) != len(tc.want) {
				t.Fatalf("parseGroupNames(%q) = %v, want %v", tc.out, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseGroupNames(%q) = %v, want %v", tc.out, got, tc.want)
				}
			}
		})
	}
}

func TestHasAdminGroup(t *testing.T) {
	for _, tc := range []struct {
		groups []string
		want   bool
	}{
		{adminGroups, true},
		{wheelGroups, true},
		{neitherGroups, false},
		{nil, false},
		{[]string{"administrators"}, false},
		{[]string{"_wheel"}, false},
	} {
		if got := hasAdminGroup(tc.groups); got != tc.want {
			t.Errorf("hasAdminGroup(%v) = %v, want %v", tc.groups, got, tc.want)
		}
	}
}

// RequireNetworkPrivilege is the dispatch, and off macOS there is nothing to
// dispatch to: Linux escalates one nmcli/ip command at a time and runs QEMU
// itself unprivileged, so demanding root up front would refuse runs that
// work. On macOS only the mode that needs nothing is asserted here, because
// what the other modes answer depends on the user running the tests.
func TestRequireNetworkPrivilege(t *testing.T) {
	if err := RequireNetworkPrivilege("user"); err != nil {
		t.Fatalf("RequireNetworkPrivilege(\"user\") = %v, want nil", err)
	}
	if runtime.GOOS == "darwin" {
		return
	}
	for _, mode := range []string{"shared", "bridged", "user", ""} {
		if err := RequireNetworkPrivilege(mode); err != nil {
			t.Errorf("RequireNetworkPrivilege(%q) on %s = %v, want nil", mode, runtime.GOOS, err)
		}
	}
}
