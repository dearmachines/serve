package volume

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type Spec struct {
	Source   string
	Target   string
	ReadOnly bool
}

func Parse(raw string) (Spec, error) {
	parts := strings.Split(raw, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return Spec{}, fmt.Errorf("entries must use source:target[:ro|rw] format")
	}

	source := strings.TrimSpace(parts[0])
	target := strings.TrimSpace(parts[1])
	if source == "" || target == "" {
		return Spec{}, fmt.Errorf("entries must use source:target format")
	}
	if !path.IsAbs(source) && !namePattern.MatchString(source) {
		return Spec{}, fmt.Errorf("source must be an absolute host path or a valid volume name")
	}
	if !path.IsAbs(target) {
		return Spec{}, fmt.Errorf("container target must be absolute")
	}

	readOnly := false
	if len(parts) == 3 {
		switch strings.TrimSpace(parts[2]) {
		case "ro":
			readOnly = true
		case "rw":
		default:
			return Spec{}, fmt.Errorf("option must be ro or rw")
		}
	}
	return Spec{Source: source, Target: target, ReadOnly: readOnly}, nil
}

func ValidateAll(raw []string) error {
	targets := map[string]bool{}
	for _, value := range raw {
		spec, err := Parse(value)
		if err != nil {
			return err
		}
		if targets[spec.Target] {
			return fmt.Errorf("target %q is configured more than once", spec.Target)
		}
		targets[spec.Target] = true
	}
	return nil
}
