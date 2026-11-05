package flywheel

import (
	"sort"
	"strings"

	"github.com/mrrobotisreal/media_manipulator_api/internal/assistant"
)

// Cluster is a group of flagged turns on one surface that retrieve the same
// top documents.
type Cluster struct {
	Key     string
	Surface string
	Docs    []string // retrieved doc ids (the key), best first order not kept: sorted
	Turns   []Flagged
	// Consulted counts docs the model read or sent users to, across the turns.
	Consulted map[string]int
	Counts    map[Signal]int
	Label     string // optional Haiku label
}

// Size is the number of turns.
func (c *Cluster) Size() int { return len(c.Turns) }

// Weight orders clusters: more turns first, then more distinct severe signals.
func (c *Cluster) weight() int {
	w := 0
	for s, n := range c.Counts {
		k := 1
		if s == SigThumbsDown || s == SigFastUndo || s == SigInvalidOps {
			k = 2
		}
		w += k * n
	}
	return w
}

// NoDocs is the key part of a turn whose message retrieved nothing.
const NoDocs = "(no docs)"

// ClusterTurns groups flagged turns by surface + the top `depth` retrieved doc
// ids for the user's message (the current corpus; the stored turn does not keep
// the ids it saw). Order is deterministic: size, weight, key.
func ClusterTurns(flagged []Flagged, r assistant.Retriever, depth int) []*Cluster {
	if depth < 1 {
		depth = 2
	}
	by := map[string]*Cluster{}
	for _, f := range flagged {
		var ids []string
		if r != nil {
			for _, h := range r.Search(f.Turn.UserText, f.Turn.Surface, depth) {
				ids = append(ids, h.Doc.ID)
			}
		}
		sort.Strings(ids)
		part := NoDocs
		if len(ids) > 0 {
			part = strings.Join(ids, " + ")
		}
		key := f.Turn.Surface + " | " + part
		c := by[key]
		if c == nil {
			c = &Cluster{Key: key, Surface: f.Turn.Surface, Docs: ids, Consulted: map[string]int{}, Counts: map[Signal]int{}}
			by[key] = c
		}
		c.Turns = append(c.Turns, f)
		for _, s := range f.Signals {
			c.Counts[s]++
		}
		for _, d := range f.Turn.ConsultedDocs() {
			c.Consulted[d]++
		}
	}
	out := make([]*Cluster, 0, len(by))
	for _, c := range by {
		sort.SliceStable(c.Turns, func(i, j int) bool {
			if !c.Turns[i].Turn.At.Equal(c.Turns[j].Turn.At) {
				return c.Turns[i].Turn.At.Before(c.Turns[j].Turn.At)
			}
			return c.Turns[i].Turn.TurnID < c.Turns[j].Turn.TurnID
		})
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Size() != out[j].Size() {
			return out[i].Size() > out[j].Size()
		}
		if out[i].weight() != out[j].weight() {
			return out[i].weight() > out[j].weight()
		}
		return out[i].Key < out[j].Key
	})
	return out
}
