package exec

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Group names the staging side of a declared path.
type Group string

const (
	GroupIn      Group = "in"
	GroupOut     Group = "out"
	GroupScratch Group = "scratch"
)

// Mapping is one declared path and where it lives on the worker.
type Mapping struct {
	Group  Group
	N      int // declaration index within the group
	Local  string
	Remote string
	Path   Path
}

// Mapper rewrites a Spec for the worker by exact declared-prefix substitution.
type Mapper struct {
	Base       string // /opt/dlami/nvme/burst/<runId>
	RemoteCard int
	Maps       []Mapping // declaration order
	byLen      []Mapping // longest local prefix first
}

// NewMapper builds the mapping: <root>/<runId>/{in,out,scratch}/<n>/<basename>.
func NewMapper(root, runID string, s Spec) (*Mapper, error) {
	if root == "" {
		root = DefaultRemoteRoot
	}
	if runID == "" || strings.ContainsAny(runID, "/.") {
		return nil, fmt.Errorf("%w: bad run id", ErrInvalidSpec)
	}
	m := &Mapper{Base: path.Join(root, runID), RemoteCard: DefaultRemoteCard}
	add := func(g Group, ps []Path) {
		for n, p := range ps {
			m.Maps = append(m.Maps, Mapping{Group: g, N: n, Local: p.Local,
				Remote: path.Join(m.Base, string(g), strconv.Itoa(n), filepath.Base(p.Local)), Path: p})
		}
	}
	add(GroupIn, s.Inputs)
	add(GroupOut, s.Outputs)
	add(GroupScratch, s.Scratch)
	m.byLen = append([]Mapping(nil), m.Maps...)
	sort.SliceStable(m.byLen, func(i, j int) bool { return len(m.byLen[i].Local) > len(m.byLen[j].Local) })
	return m, nil
}

// Find returns the mapping for a group and index.
func (m *Mapper) Find(g Group, n int) (Mapping, bool) {
	for _, mp := range m.Maps {
		if mp.Group == g && mp.N == n {
			return mp, true
		}
	}
	return Mapping{}, false
}

// RewriteString substitutes every declared local path with its worker path,
// longest prefix first. Nothing outside a declared prefix is touched.
func (m *Mapper) RewriteString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		hit := false
		for _, mp := range m.byLen {
			if matchAt(s, i, mp.Local) {
				b.WriteString(mp.Remote)
				i += len(mp.Local)
				hit = true
				break
			}
		}
		if !hit {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

func (m *Mapper) rewriteCard(c Cmd) (Cmd, error) {
	ca := c.Card
	if ca == nil {
		return c, nil
	}
	local, remote := strconv.Itoa(ca.Local), strconv.Itoa(m.RemoteCard)
	for _, key := range ca.Env {
		found := false
		for i, kv := range c.Env {
			if strings.HasPrefix(kv, key+"=") {
				if kv[len(key)+1:] != local {
					return c, fmt.Errorf("%w: %s is %q, declared card %s", ErrCardArgs, key, kv[len(key)+1:], local)
				}
				c.Env[i] = key + "=" + remote
				found = true
			}
		}
		if !found {
			return c, fmt.Errorf("%w: env %s not present", ErrCardArgs, key)
		}
	}
	for _, flag := range ca.Flags {
		found := false
		for i := 0; i < len(c.Args); i++ {
			switch {
			case c.Args[i] == flag && i+1 < len(c.Args):
				if c.Args[i+1] != local {
					return c, fmt.Errorf("%w: %s is %q, declared card %s", ErrCardArgs, flag, c.Args[i+1], local)
				}
				c.Args[i+1] = remote
				found = true
				i++
			case strings.HasPrefix(c.Args[i], flag+"="):
				if c.Args[i][len(flag)+1:] != local {
					return c, fmt.Errorf("%w: %s is %q, declared card %s", ErrCardArgs, flag, c.Args[i][len(flag)+1:], local)
				}
				c.Args[i] = flag + "=" + remote
				found = true
			}
		}
		if !found {
			return c, fmt.Errorf("%w: flag %s not present", ErrCardArgs, flag)
		}
	}
	return c, nil
}

// RewriteCmd returns a copy of c with Args, Env and Dir rewritten and the card
// substituted. The input is not modified.
func (m *Mapper) RewriteCmd(c Cmd) (Cmd, error) {
	out := c
	out.Args = append([]string(nil), c.Args...)
	out.Env = append([]string(nil), c.Env...)
	for i := range out.Args {
		out.Args[i] = m.RewriteString(out.Args[i])
	}
	for i := range out.Env {
		out.Env[i] = m.RewriteString(out.Env[i])
	}
	out.Dir = m.RewriteString(out.Dir)
	out.Exe = m.RewriteString(out.Exe)
	return m.rewriteCard(out)
}

// RewriteSteps rewrites every step and re-checks that no data-root path is left
// behind (defence in depth behind Validate).
func (m *Mapper) RewriteSteps(v Validator, steps []Cmd) ([]Cmd, error) {
	out := make([]Cmd, 0, len(steps))
	for i, c := range steps {
		r, err := m.RewriteCmd(c)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		strs := append([]string{r.Exe, r.Dir}, r.Args...)
		strs = append(strs, r.Env...)
		for _, s := range strs {
			if bad := v.uncovered(s, nil); bad != "" {
				return nil, fmt.Errorf("%w: step %d still references %s after mapping", ErrUndeclaredPath, i, bad)
			}
		}
		out = append(out, r)
	}
	return out, nil
}
