package gateway

import "golang.org/x/sys/unix"

func flushSetupInput(fd int) error {
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
