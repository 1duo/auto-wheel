package accountservice

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func authenticatePeer(conn net.Conn, release string) error {
	u, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("Unix peer required")
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return err
	}
	var credentials *unix.Ucred
	var problem error
	if err = raw.Control(func(fd uintptr) {
		credentials, problem = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if problem != nil || credentials == nil || credentials.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("peer UID mismatch")
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/exe", credentials.Pid))
	if err != nil {
		return err
	}
	defer file.Close()
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return err
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != release {
		return fmt.Errorf("peer executable release mismatch")
	}
	return nil
}
