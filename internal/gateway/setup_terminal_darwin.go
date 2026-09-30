package gateway

import "golang.org/x/sys/unix"

func flushSetupInput(fd int) error {
	// Darwin TIOCFLUSH takes an int pointer containing FREAD (sys/fcntl.h).
	const fread = 1
	return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, fread)
}
