package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWithUserRWFromPlainMode(t *testing.T) {
	got := withUserRW(nil, 0o600, 1000)
	want := []aclEntry{
		{aclUserObj, 6, 0},
		{aclUser, 6, 1000},
		{aclGroupObj, 0, 0},
		{aclMask, 6, 0},
		{aclOther, 0, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestWithUserRWKeepsOtherEntries(t *testing.T) {
	existing := []aclEntry{
		{aclUserObj, 6, 0}, {aclUser, 4, 1000}, {aclGroupObj, 0, 0}, {aclMask, 4, 0}, {aclOther, 0, 0},
	}
	got := withUserRW(existing, 0o600, 1000)
	if !hasUserRW(got, 1000) {
		t.Errorf("uid 1000 not upgraded to rw: %v", got)
	}
	for _, e := range got {
		if e.tag == aclMask && e.perm != 6 {
			t.Errorf("mask not recomputed: %+v", e)
		}
	}
	if len(got) != len(existing) {
		t.Errorf("entry count changed: %v", got)
	}
}

func TestACLRoundTrip(t *testing.T) {
	es := withUserRW(nil, 0o640, 4242)
	back, err := parseACL(encodeACL(es))
	if err != nil || len(back) != len(es) {
		t.Fatalf("round trip: %v %v", back, err)
	}
	for i := range es {
		if es[i] != back[i] {
			t.Errorf("entry %d changed: %+v vs %+v", i, es[i], back[i])
		}
	}
	if _, err := parseACL([]byte{1, 0, 0, 0}); err == nil {
		t.Error("wrong ACL version accepted")
	}
}

// The kernel validates the ACL we build, so exercise it on a file we own (no root needed).
func TestGrantAndRevokeOnRealFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := grantAccess(path, 54321)
	if errors.Is(err, syscall.EOPNOTSUPP) || (err != nil && errors.Is(errors.Unwrap(err), syscall.EOPNOTSUPP)) {
		t.Skip("filesystem has no POSIX ACL support")
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err := getACL(path)
	if err != nil {
		t.Fatal(err)
	}
	es, _ := parseACL(raw)
	if !hasUserRW(es, 54321) {
		t.Fatalf("grant not applied: %v", es)
	}
	// second grant is a no-op and must not lose the first restore info
	again, err := grantAccess(path, 54321)
	if err != nil || again.changed {
		t.Fatalf("regrant: changed=%v err=%v", again != nil && again.changed, err)
	}
	if err := g.revoke(); err != nil {
		t.Fatal(err)
	}
	if _, err := getACL(path); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("ACL should be gone after revoke, got %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode not restored: %v", st.Mode().Perm())
	}
}

func TestRevokeRestoresPreviousACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node")
	os.WriteFile(path, nil, 0o600)
	first, err := grantAccess(path, 1111)
	if err != nil {
		t.Skip("no ACL support:", err)
	}
	before, _ := getACL(path)
	second, err := grantAccess(path, 2222)
	if err != nil || !second.changed {
		t.Fatalf("second grant: %v", err)
	}
	if err := second.revoke(); err != nil {
		t.Fatal(err)
	}
	after, _ := getACL(path)
	if string(before) != string(after) {
		t.Errorf("ACL after revoke differs:\n before % x\n after  % x", before, after)
	}
	first.revoke()
}

func TestInvokingUser(t *testing.T) {
	t.Setenv("SUDO_UID", "")
	t.Setenv("PKEXEC_UID", "")
	if _, _, err := invokingUser(""); err == nil {
		t.Error("root without sudo context must be refused")
	}
	self := os.Getenv("USER")
	if self == "" || self == "root" {
		t.Skip("needs a non-root login user")
	}
	uid, _, err := invokingUser(self)
	if err != nil || uid == 0 {
		t.Errorf("-user %s: uid=%d err=%v", self, uid, err)
	}
	t.Setenv("SUDO_UID", "0")
	if _, _, err := invokingUser(""); err == nil {
		t.Error("SUDO_UID=0 must be refused (never serve the network as root)")
	}
}
