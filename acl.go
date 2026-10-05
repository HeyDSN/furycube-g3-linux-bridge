package main

// Temporary per-user access to a hidraw node via a POSIX ACL, so the bridge can run as a
// normal user while only a short-lived root parent touches device permissions.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"syscall"
)

const aclXattr = "system.posix_acl_access"

const (
	aclVersion  = 2
	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20
	permRW      = 0x06
)

type aclEntry struct {
	tag  uint16
	perm uint16
	id   uint32
}

func parseACL(b []byte) ([]aclEntry, error) {
	if len(b) < 4 || binary.LittleEndian.Uint32(b) != aclVersion || (len(b)-4)%8 != 0 {
		return nil, errors.New("unsupported ACL encoding")
	}
	var es []aclEntry
	for i := 4; i < len(b); i += 8 {
		es = append(es, aclEntry{
			tag:  binary.LittleEndian.Uint16(b[i:]),
			perm: binary.LittleEndian.Uint16(b[i+2:]),
			id:   binary.LittleEndian.Uint32(b[i+4:]),
		})
	}
	return es, nil
}

func encodeACL(es []aclEntry) []byte {
	b := binary.LittleEndian.AppendUint32(nil, aclVersion)
	for _, e := range es {
		b = binary.LittleEndian.AppendUint16(b, e.tag)
		b = binary.LittleEndian.AppendUint16(b, e.perm)
		b = binary.LittleEndian.AppendUint32(b, e.id)
	}
	return b
}

// withUserRW returns the ACL that adds read/write for uid on top of existing (nil means the
// node only has plain mode bits). The kernel wants entries sorted by tag, then id.
func withUserRW(existing []aclEntry, mode os.FileMode, uid uint32) []aclEntry {
	es := append([]aclEntry(nil), existing...)
	if len(es) == 0 {
		es = []aclEntry{
			{tag: aclUserObj, perm: uint16(mode>>6) & 7},
			{tag: aclGroupObj, perm: uint16(mode>>3) & 7},
			{tag: aclOther, perm: uint16(mode) & 7},
		}
	}
	found := false
	for i := range es {
		if es[i].tag == aclUser && es[i].id == uid {
			es[i].perm |= permRW
			found = true
		}
	}
	if !found {
		es = append(es, aclEntry{tag: aclUser, perm: permRW, id: uid})
	}
	var mask uint16
	maskAt := -1
	for i, e := range es {
		switch e.tag {
		case aclUser, aclGroupObj, aclGroup:
			mask |= e.perm
		case aclMask:
			maskAt = i
		}
	}
	if maskAt >= 0 {
		es[maskAt].perm = mask
	} else {
		es = append(es, aclEntry{tag: aclMask, perm: mask})
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].tag != es[j].tag {
			return es[i].tag < es[j].tag
		}
		return es[i].id < es[j].id
	})
	return es
}

func hasUserRW(es []aclEntry, uid uint32) bool {
	for _, e := range es {
		if e.tag == aclUser && e.id == uid && e.perm&permRW == permRW {
			return true
		}
	}
	return false
}

// aclGrant remembers what a node looked like before the grant, to put it back afterwards.
type aclGrant struct {
	path     string
	changed  bool
	origACL  []byte // nil: the node had no extended ACL
	origMode os.FileMode
}

func getACL(path string) ([]byte, error) {
	n, err := syscall.Getxattr(path, aclXattr, nil)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	n, err = syscall.Getxattr(path, aclXattr, buf)
	return buf[:n], err
}

// grantAccess gives uid read/write on path, unless it already has it.
func grantAccess(path string, uid uint32) (*aclGrant, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	g := &aclGrant{path: path, origMode: st.Mode().Perm()}
	var existing []aclEntry
	switch raw, err := getACL(path); {
	case err == nil:
		g.origACL = raw
		if existing, err = parseACL(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	case !errors.Is(err, syscall.ENODATA) && !errors.Is(err, syscall.EOPNOTSUPP):
		return nil, fmt.Errorf("%s: read ACL: %w", path, err)
	}
	if hasUserRW(existing, uid) {
		return g, nil
	}
	if err := syscall.Setxattr(path, aclXattr, encodeACL(withUserRW(existing, g.origMode, uid)), 0); err != nil {
		return nil, fmt.Errorf("%s: set ACL: %w", path, err)
	}
	g.changed = true
	return g, nil
}

// revoke restores the node's previous permissions.
func (g *aclGrant) revoke() error {
	if !g.changed {
		return nil
	}
	if g.origACL != nil {
		// the saved ACL carries its own mask, so no chmod (it would rewrite the mask)
		return syscall.Setxattr(g.path, aclXattr, g.origACL, 0)
	}
	if err := syscall.Removexattr(g.path, aclXattr); err != nil {
		return err
	}
	return os.Chmod(g.path, g.origMode)
}
