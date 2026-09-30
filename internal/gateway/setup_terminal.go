//go:build linux || darwin

package gateway

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// readSetupSecret consumes exactly one line. Keeping terminal reads in this
// goroutine lets interruption restore echo before returning, without a blocked
// ReadPassword goroutine consuming a later prompt's input.
func readSetupSecret(in io.Reader, out io.Writer) (secret string, err error) {
	file, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return readSetupLine(in)
	}
	fd := int(file.Fd())
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT, unix.SIGTSTP)
	defer signal.Stop(interrupts)
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("hide terminal input: %w", err)
	}
	defer func() {
		// Raw mode disables the terminal's usual Ctrl-C queue flush. Discard
		// pending paste bytes on failure so they cannot reach the next prompt.
		if err != nil {
			if flushErr := flushSetupInput(fd); flushErr != nil {
				err = errors.Join(err, fmt.Errorf("clear canceled terminal input: %w", flushErr))
			}
		}
		restoreErr := term.Restore(fd, state)
		_, newlineErr := fmt.Fprintln(out)
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore terminal input: %w", restoreErr))
		}
		if newlineErr != nil {
			err = errors.Join(err, newlineErr)
		}
		if err != nil {
			secret = ""
		}
	}()

	const maxLength = 64 * 1024
	value := make([]byte, 0, 256)
	defer func() { clear(value) }()
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	var input [1]byte
	for {
		select {
		case <-interrupts:
			return "", errors.New("input canceled")
		default:
		}
		ready, pollErr := unix.Poll(poll, 100)
		if errors.Is(pollErr, unix.EINTR) {
			continue
		}
		if pollErr != nil {
			return "", fmt.Errorf("read terminal input: %w", pollErr)
		}
		if ready == 0 {
			continue
		}
		select {
		case <-interrupts:
			return "", errors.New("input canceled")
		default:
		}
		n, readErr := unix.Read(fd, input[:])
		if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) {
			continue
		}
		if readErr != nil {
			return "", fmt.Errorf("read terminal input: %w", readErr)
		}
		if n == 0 {
			return "", io.EOF
		}
		switch input[0] {
		case '\r', '\n':
			return string(value), nil
		case 3, 26, 28: // Ctrl-C, Ctrl-Z, Ctrl-\.
			return "", errors.New("input canceled")
		case 4: // Ctrl-D cancels even a partially entered key.
			return "", io.EOF
		case 8, 127: // Backspace and Delete.
			_, width := utf8.DecodeLastRune(value)
			clear(value[len(value)-width:])
			value = value[:len(value)-width]
		case 21: // Ctrl-U clears the current line.
			clear(value)
			value = value[:0]
		default:
			if input[0] < 32 {
				return "", errors.New("API key input contains a control character")
			}
			if len(value) >= maxLength {
				return "", errors.New("input exceeds 64 KiB")
			}
			value = append(value, input[0])
		}
	}
}
