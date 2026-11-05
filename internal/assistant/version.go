package assistant

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"sort"
	"sync"
)

// PromptVersion is the content hash of everything model-facing that ships in
// the binary (MM113): the persona, every surface intro and capsule, the
// embedded knowledge index and every op schema file. It is the first 12 hex
// characters of a SHA-256, computed once at startup, and is stored on every
// assistant message and usage row, returned by capabilities, sent in the meta
// event and recorded with every eval run. Any edit to those inputs changes it,
// so a regression can be pinned to the change that caused it.
//
// MM114 and MM115 add model-facing text; keep their inputs inside this hash
// (persona, surfaceIntro, capsules, knowledge/, ops/schema/).
func PromptVersion() string {
	versionOnce.Do(func() { versionValue = computeEmbeddedPromptVersion() })
	return versionValue
}

var (
	versionOnce  sync.Once
	versionValue string
)

// PromptVersionParts are the inputs of the hash.
type PromptVersionParts struct {
	Persona string
	// Intros and Capsules are keyed by surface.
	Intros   map[string]string
	Capsules map[string]string
	// KnowledgeIndex is knowledge/index.json verbatim.
	KnowledgeIndex []byte
	// Schemas is every op schema file, keyed by file name.
	Schemas map[string][]byte
}

// HashPromptVersion hashes the parts in a fixed order, each field length
// framed so that moving bytes between fields changes the digest.
func HashPromptVersion(p PromptVersionParts) string {
	h := sha256.New()
	put := func(label string, b []byte) {
		h.Write([]byte(label))
		h.Write([]byte{0})
		var n [8]byte
		l := uint64(len(b))
		for i := 0; i < 8; i++ {
			n[i] = byte(l >> (8 * i))
		}
		h.Write(n[:])
		h.Write(b)
	}
	put("persona", []byte(p.Persona))
	for _, k := range sortedKeys(p.Intros) {
		put("intro:"+k, []byte(p.Intros[k]))
	}
	for _, k := range sortedKeys(p.Capsules) {
		put("capsule:"+k, []byte(p.Capsules[k]))
	}
	put("knowledge-index", p.KnowledgeIndex)
	names := make([]string, 0, len(p.Schemas))
	for k := range p.Schemas {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		put("schema:"+k, p.Schemas[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func computeEmbeddedPromptVersion() string {
	p := PromptVersionParts{Persona: persona, Intros: surfaceIntro, Capsules: map[string]string{}, Schemas: map[string][]byte{}}
	if k, err := NewEmbeddedKnowledge(); err == nil {
		for _, s := range []string{SurfaceGuide, SurfaceStudio, SurfaceVector, SurfacePhoto} {
			p.Capsules[s] = k.Capsule(s)
		}
	}
	p.KnowledgeIndex, _ = fs.ReadFile(knowledgeFS, "knowledge/index.json")
	if entries, err := fs.ReadDir(opsFS, "ops/schema"); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if b, err := fs.ReadFile(opsFS, "ops/schema/"+e.Name()); err == nil {
				p.Schemas[e.Name()] = b
			}
		}
	}
	return HashPromptVersion(p)
}
