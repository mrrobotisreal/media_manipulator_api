// Package exec is the GPU burst executor seam (MM125, design §7.1): a Spec
// declares one GPU step's commands plus every path it touches, a Local executor
// runs it exactly as today, and a Remote executor moves the declared files
// through S3 and runs the same commands on a burst worker. It fails closed: a
// path under a configured data root that the Spec did not declare is an error,
// never a guess.
package exec

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// Errors callers branch on.
var (
	// ErrUndeclaredPath: an arg, env value, dir or exe names a path under a
	// data root (UPLOAD_DIR, OUTPUT_DIR, TEMP_DIR, PHOTO_DIR, the AI tmp dir)
	// that is not under a declared Input, Output or Scratch path.
	ErrUndeclaredPath = errors.New("burst exec: undeclared path")
	// ErrInvalidSpec: a structural problem (relative or duplicate declaration).
	ErrInvalidSpec = errors.New("burst exec: invalid spec")
	// ErrCardArgs: a CardArgs declaration that does not match the command.
	ErrCardArgs = errors.New("burst exec: card args mismatch")
)

// Path is one declared local path (a file or a directory).
type Path struct {
	Local string
	// Dir marks an Output or Scratch path as a directory the target creates
	// before the steps run. Inputs are stat'ed instead.
	Dir bool
	// Optional marks an Output the command may legitimately not produce.
	Optional bool
}

// P declares a file path; D declares a directory path.
func P(local string) Path { return Path{Local: local} }
func D(local string) Path { return Path{Local: local, Dir: true} }

// CardArgs declares where the tool put the GPU index, so the remote executor
// can rewrite it to the worker's card. There is no pattern guessing: what is
// not declared is not touched, and a declaration that does not match the
// command is an error.
type CardArgs struct {
	// Local is the card index the tool wrote (the value to replace).
	Local int
	// Env are env keys whose value is the card (CUDA_VISIBLE_DEVICES).
	Env []string
	// Flags are arg flags followed by the card as the next arg ("-g",
	// "--gpu", "--device_index"); "flag=card" is recognised too.
	Flags []string
}

// Standard card spellings from the tools.
const (
	CardEnvCUDA        = "CUDA_VISIBLE_DEVICES"
	CardFlagG          = "-g"
	CardFlagGPU        = "--gpu"
	CardFlagDeviceIdx  = "--device_index"
	DefaultRemoteCard  = 0
	DefaultRemoteRoot  = "/opt/dlami/nvme/burst"
	aiTmpRoot          = "/opt/media-manipulator-ai/tmp"
	runtimePrefixAIDir = "/opt/media-manipulator-ai/"
)

// Cmd is one command. Env entries are appended to the inherited environment
// (the same meaning as AIService.exec and cmdaudit.Spec.ExtraEnv).
type Cmd struct {
	// Label is the AI exec label / audit stage.
	Label   string
	Exe     string
	Args    []string
	Env     []string
	Dir     string
	Timeout time.Duration
	Card    *CardArgs
}

// Spec is one GPU step: commands run in order on one target.
type Spec struct {
	Workload  string // profile key
	Tool      string // audit tool label
	JobID     string
	RequestID string
	Steps     []Cmd
	Inputs    []Path // must exist at the target before Steps
	Outputs   []Path // brought back to their original local paths
	Scratch   []Path // created empty at the target, never returned
	// Stdout receives PROGRESS lines etc., line by line.
	Stdout io.Writer
}

// allowedRuntime are the /opt/media-manipulator-ai subtrees the image
// reproduces (decision 1). tmp is a data root, not a runtime path.
var allowedRuntime = map[string]bool{"bin": true, "env": true, "models": true, "scripts": true, "venvs": true, "vulkan": true, "repos": true}

// Validator knows the data roots. Roots must be absolute (use RootsFrom).
type Validator struct{ Roots []string }

// RootsFrom builds the data-root list from the configured dirs (empty ones are
// skipped; relative ones are made absolute the way cmdaudit does).
func RootsFrom(upload, output, temp, photo string) []string {
	var out []string
	for _, p := range []string{upload, output, temp, photo, aiTmpRoot} {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

func isNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// boundaryAfter: position i ends a path token.
func boundaryAfter(s string, i int) bool {
	return i >= len(s) || s[i] == '/' || !isNameChar(s[i])
}

// boundaryBefore: position i starts a path token.
func boundaryBefore(s string, i int) bool {
	return i == 0 || !(isNameChar(s[i-1]) || s[i-1] == '/')
}

// matchAt reports whether declared path d sits at s[i:] as a whole path prefix.
func matchAt(s string, i int, d string) bool {
	return d != "" && strings.HasPrefix(s[i:], d) && boundaryAfter(s, i+len(d)) && boundaryBefore(s, i)
}

func declared(s Spec) []string {
	var out []string
	for _, group := range [][]Path{s.Inputs, s.Outputs, s.Scratch} {
		for _, p := range group {
			out = append(out, p.Local)
		}
	}
	return out
}

// uncovered returns the first root occurrence in str that no declared path
// covers, or "" when everything is covered.
func (v Validator) uncovered(str string, decl []string) string {
	check := func(root string, needSeg bool) string {
		for i := 0; i < len(str); {
			j := strings.Index(str[i:], root)
			if j < 0 {
				break
			}
			pos := i + j
			end := pos + len(root)
			i = end
			if needSeg {
				seg := str[end:]
				k := 0
				for k < len(seg) && isNameChar(seg[k]) {
					k++
				}
				if allowedRuntime[seg[:k]] || seg[:k] == "" {
					continue
				}
				end += k
			} else if !boundaryAfter(str, end) {
				continue
			}
			covered := false
			for _, d := range decl {
				if matchAt(str, pos, d) {
					covered = true
					break
				}
			}
			if !covered {
				return str[pos:end]
			}
		}
		return ""
	}
	for _, r := range v.Roots {
		if r == "" {
			continue
		}
		if bad := check(r, false); bad != "" {
			return bad
		}
	}
	return check(strings.TrimSuffix(runtimePrefixAIDir, "/")+"/", true)
}

// Validate runs before any execution, locally or remotely.
func (v Validator) Validate(s Spec) error {
	if len(s.Steps) == 0 {
		return fmt.Errorf("%w: no steps", ErrInvalidSpec)
	}
	seen := map[string]bool{}
	for kind, group := range map[string][]Path{"input": s.Inputs, "output": s.Outputs, "scratch": s.Scratch} {
		for _, p := range group {
			if !filepath.IsAbs(p.Local) || filepath.Clean(p.Local) != p.Local || p.Local == "/" {
				return fmt.Errorf("%w: %s %q must be absolute and clean", ErrInvalidSpec, kind, p.Local)
			}
			if seen[p.Local] {
				return fmt.Errorf("%w: %q declared twice", ErrInvalidSpec, p.Local)
			}
			seen[p.Local] = true
		}
	}
	decl := declared(s)
	for si, c := range s.Steps {
		if c.Exe == "" {
			return fmt.Errorf("%w: step %d has no executable", ErrInvalidSpec, si)
		}
		strs := append([]string{c.Exe, c.Dir}, c.Args...)
		strs = append(strs, c.Env...)
		for _, str := range strs {
			if bad := v.uncovered(str, decl); bad != "" {
				// The offending path is a data root, never user content.
				return fmt.Errorf("%w: step %d references a path under %s that is not declared", ErrUndeclaredPath, si, bad)
			}
		}
	}
	return nil
}
