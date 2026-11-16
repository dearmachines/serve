package cli

import (
	"fmt"
	"io"
	"os"
	"unicode/utf8"
)

// WithTerminalDetector overrides how the CLI decides whether stdout is an
// interactive terminal. Tests use it to exercise terminal-only behavior.
func WithTerminalDetector(detect func(io.Writer) bool) Option {
	return func(c *Command) {
		if detect != nil {
			c.isTerminal = detect
		}
	}
}

func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// workloadOutput wraps stdout for bytes that come from a workload (logs,
// exec output). On a terminal, control sequences are neutralized so a
// workload cannot rewrite the screen, set the title, or write the
// operator's clipboard (OSC 52). Piped output and --raw stay byte-exact.
func workloadOutput(stdout io.Writer, raw bool, detect func(io.Writer) bool) io.Writer {
	if raw || detect == nil || !detect(stdout) {
		return stdout
	}
	return newTerminalSanitizer(stdout)
}

// takeRawFlag removes --raw from the command's own flags. Arguments after
// "--" belong to the workload command and are never inspected.
func takeRawFlag(args []string) (bool, []string) {
	raw := false
	rest := make([]string, 0, len(args))
	for i, arg := range args {
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if arg == "--raw" {
			raw = true
			continue
		}
		rest = append(rest, arg)
	}
	return raw, rest
}

// terminalSanitizer renders C0/C1 control characters visibly (ESC as ^[)
// while keeping newline, carriage return, and tab. It buffers a UTF-8
// sequence split across writes so multibyte text is never corrupted.
type terminalSanitizer struct {
	out     io.Writer
	pending []byte
}

func newTerminalSanitizer(out io.Writer) *terminalSanitizer {
	return &terminalSanitizer{out: out}
}

func (s *terminalSanitizer) Write(p []byte) (int, error) {
	data := append(s.pending, p...)
	s.pending = nil
	var clean []byte
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size <= 1 {
			if !utf8.FullRune(data) {
				s.pending = append([]byte(nil), data...)
				break
			}
			clean = utf8.AppendRune(clean, utf8.RuneError)
			data = data[1:]
			continue
		}
		clean = appendVisible(clean, r)
		data = data[size:]
	}
	if _, err := s.out.Write(clean); err != nil {
		return 0, err
	}
	return len(p), nil
}

func appendVisible(dst []byte, r rune) []byte {
	switch {
	case r == '\n' || r == '\r' || r == '\t':
		return append(dst, byte(r))
	case r < 0x20:
		return append(dst, '^', byte(r)+0x40)
	case r == 0x7f:
		return append(dst, '^', '?')
	case r >= 0x80 && r <= 0x9f:
		return fmt.Appendf(dst, "\\u%04x", r)
	default:
		return utf8.AppendRune(dst, r)
	}
}
