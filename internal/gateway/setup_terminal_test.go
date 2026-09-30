//go:build linux || darwin

package gateway

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSetupSecretPipedInputDoesNotConsumeTheNextPrompt(t *testing.T) {
	in := strings.NewReader("fake-private-key\nnext-answer\n")
	var out bytes.Buffer
	got, err := readSetupSecret(in, &out)
	if err != nil || got != "fake-private-key" {
		t.Fatalf("secret input failed: %v", err)
	}
	next, err := readSetupLine(in)
	if err != nil || next != "next-answer" {
		t.Fatalf("next prompt was consumed: %q, %v", next, err)
	}
	if out.Len() != 0 {
		t.Fatal("piped secret input printed output")
	}
}

type setupBrokenReader struct{}

func (setupBrokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestSetupSecretPipedInputErrors(t *testing.T) {
	for name, in := range map[string]io.Reader{
		"empty":  strings.NewReader(""),
		"broken": setupBrokenReader{},
		"long":   strings.NewReader(strings.Repeat("x", 64*1024+1) + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readSetupSecret(in, io.Discard)
			if err == nil || got != "" {
				t.Fatal("incomplete or oversized input returned a usable secret")
			}
			if name == "broken" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("reader error was lost: %v", err)
			}
		})
	}
}
