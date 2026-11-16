package cli

import (
	"bytes"
	"testing"
)

func TestTerminalSanitizerNeutralizesEscapeSequences(t *testing.T) {
	var out bytes.Buffer
	w := newTerminalSanitizer(&out)
	// OSC 52 clipboard write, screen clear, title set, and a C1 CSI.
	input := "ok\x1b]52;c;ZXZpbA==\a\x1b[2J\x1b]0;pwned\x07tab\there\r\nc1:\u009b31m done\n"
	for _, chunk := range [][]byte{[]byte(input[:5]), []byte(input[5:47]), []byte(input[47:])} {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}

	got := out.String()
	for _, forbidden := range []string{"\x1b", "\a", "\u009b"} {
		if bytes.Contains(out.Bytes(), []byte(forbidden)) {
			t.Fatalf("sanitized output still contains %q: %q", forbidden, got)
		}
	}
	for _, kept := range []string{"ok", "^[]52;c;ZXZpbA==^G", "tab\there\r\n", "done\n"} {
		if !bytes.Contains(out.Bytes(), []byte(kept)) {
			t.Fatalf("sanitized output lost %q: %q", kept, got)
		}
	}
}

func TestTerminalSanitizerKeepsMultibyteTextSplitAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := newTerminalSanitizer(&out)
	text := []byte("héllo — ✓\n")
	for i := range text {
		if _, err := w.Write(text[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != string(text) {
		t.Fatalf("multibyte text = %q, want %q", out.String(), text)
	}
}
