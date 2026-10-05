package main

// Root side of `sudo ./furycube-g3-linux-bridge`. Root only grants the invoking user temporary access
// to the mouse's hidraw node and restarts nothing else: the bridge itself (the part that
// listens on a network port) runs as the invoking user in a child process.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

const grantInterval = 500 * time.Millisecond

// invokingUser works out who ran sudo/pkexec (or who -user names).
func invokingUser(flagUser string) (uid, gid uint32, err error) {
	var u *user.User
	switch {
	case flagUser != "":
		if u, err = user.Lookup(flagUser); err != nil {
			u, err = user.LookupId(flagUser)
		}
	case os.Getenv("SUDO_UID") != "":
		u, err = user.LookupId(os.Getenv("SUDO_UID"))
	case os.Getenv("PKEXEC_UID") != "":
		u, err = user.LookupId(os.Getenv("PKEXEC_UID"))
	default:
		return 0, 0, errors.New("running as root but cannot tell which user to serve; use sudo from your own account or pass -user NAME")
	}
	if err != nil {
		return 0, 0, err
	}
	id, _ := strconv.ParseUint(u.Uid, 10, 32)
	gd, _ := strconv.ParseUint(u.Gid, 10, 32)
	if id == 0 {
		return 0, 0, errors.New("refusing to run the network-facing bridge as root; use sudo from a normal account or pass -user NAME")
	}
	return uint32(id), uint32(gd), nil
}

func stillGranted(path string, uid uint32) bool {
	raw, err := getACL(path)
	if err != nil {
		return false
	}
	es, err := parseACL(raw)
	return err == nil && hasUserRW(es, uid)
}

// runBroker starts the unprivileged bridge, keeps the mouse's node accessible to its user
// while it runs, and puts the permissions back when it exits. It returns the exit code.
func runBroker(flagUser string) int {
	uid, gid, err := invokingUser(flagUser)
	if err != nil {
		fmt.Fprintln(os.Stderr, "furycube-g3-linux-bridge:", err)
		return 1
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "furycube-g3-linux-bridge:", err)
		return 1
	}
	// Pdeathsig fires when the creating *thread* exits, so pin this goroutine to its thread.
	runtime.LockOSThread()
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
		Pdeathsig:  syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "furycube-g3-linux-bridge: cannot start unprivileged bridge:", err)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	grants := map[string]*aclGrant{}
	defer func() {
		for path, g := range grants {
			if err := g.revoke(); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOENT) {
				log.Printf("[root] could not restore permissions on %s: %v", path, err)
			} else if g.changed {
				log.Printf("[root] restored permissions on %s", path)
			}
		}
	}()

	tick := time.NewTicker(grantInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			info := findDevice(sysHidraw)
			if info == nil {
				continue
			}
			if _, ok := grants[info.path]; ok {
				if stillGranted(info.path, uid) {
					continue
				}
				// the node was recreated (replug): it is a fresh file, nothing to restore
				delete(grants, info.path)
			}
			g, err := grantAccess(info.path, uid)
			if err != nil {
				log.Printf("[root] %v", err)
				continue
			}
			grants[info.path] = g
			if g.changed {
				log.Printf("[root] gave uid %d access to %s until the bridge exits", uid, info.path)
			}
		case s := <-sigs:
			cmd.Process.Signal(s)
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode()
			}
			if err != nil {
				return 1
			}
			return 0
		}
	}
}
