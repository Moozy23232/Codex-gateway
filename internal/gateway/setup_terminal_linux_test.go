//go:build linux

package gateway

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// This is a normal test subprocess so the package's lifecycle TestMain remains
// the only TestMain. No additional helper binary or system utility is needed.
func TestSetupTerminalProcess(t *testing.T) {
	if os.Getenv("CODEX_GATEWAY_TERMINAL_TEST") != "1" {
		return
	}
	value, err := readSetupSecret(os.Stdin, os.Stdout)
	if os.Getenv("CODEX_GATEWAY_TERMINAL_ERROR") == "1" {
		if err == nil || value != "" {
			t.Fatal("canceled input returned a usable secret")
		}
	} else if err != nil || value != os.Getenv("CODEX_GATEWAY_TERMINAL_EXPECT") {
		t.Fatalf("terminal read failed: %v", err)
	}
	fmt.Println("TERMINAL_DONE")
}

func setupPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	master := os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func TestSetupTerminalHidesInputAndRestoresAfterCancellation(t *testing.T) {
	for _, test := range []struct {
		name, input, expected string
		signal                os.Signal
		wantError             bool
	}{
		{name: "complete", input: "hidden-setup-key\n", expected: "hidden-setup-key"},
		{name: "editing", input: "discard\x15hidden-x\bkey\r", expected: "hidden-key"},
		{name: "ctrl-c", input: "partial-key\x03", wantError: true},
		{name: "canceled paste", input: "partial-key\x03paste-tail\n", wantError: true},
		{name: "ctrl-d", input: "partial-key\x04", wantError: true},
		{name: "sigint", input: "partial-key", signal: os.Interrupt, wantError: true},
		{name: "sigterm", input: "partial-key", signal: syscall.SIGTERM, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			master, slave := setupPTY(t)
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestSetupTerminalProcess$")
			cmd.Env = append(os.Environ(), "CODEX_GATEWAY_TERMINAL_TEST=1", "CODEX_GATEWAY_TERMINAL_EXPECT="+test.expected)
			if test.wantError {
				cmd.Env = append(cmd.Env, "CODEX_GATEWAY_TERMINAL_ERROR=1")
			}
			cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			ready := false
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				state, err := term.GetState(int(slave.Fd()))
				if err == nil && !reflect.DeepEqual(before, state) {
					ready = true
					break
				}
				select {
				case err := <-done:
					t.Fatalf("terminal helper exited before readiness: %v", err)
				default:
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !ready {
				t.Fatal("terminal helper did not enter hidden input mode")
			}
			if _, err := master.Write([]byte(test.input)); err != nil {
				t.Fatal(err)
			}
			if test.signal != nil {
				if err := cmd.Process.Signal(test.signal); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("terminal helper failed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal input did not finish after input or cancellation")
			}
			after, err := term.GetState(int(slave.Fd()))
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("terminal settings were not restored: %v", err)
			}
			if pending, err := unix.IoctlGetInt(int(slave.Fd()), unix.TIOCINQ); err != nil || pending != 0 {
				t.Fatalf("terminal retained input for a later prompt: bytes=%d, error=%v", pending, err)
			}
			var transcript bytes.Buffer
			for {
				poll := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
				if n, err := unix.Poll(poll, 10); err != nil || n == 0 {
					break
				}
				var buf [1024]byte
				n, err := unix.Read(int(master.Fd()), buf[:])
				if err != nil || n == 0 {
					break
				}
				transcript.Write(buf[:n])
			}
			if !bytes.Contains(transcript.Bytes(), []byte("TERMINAL_DONE")) {
				t.Fatal("terminal helper did not complete its assertions")
			}
			for _, fragment := range []string{"hidden-", "partial-key", "discard"} {
				if bytes.Contains(transcript.Bytes(), []byte(fragment)) {
					t.Fatal("terminal echoed secret input")
				}
			}
		})
	}
}
