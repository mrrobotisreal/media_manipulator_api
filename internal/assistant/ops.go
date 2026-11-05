package assistant

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Op schemas (design §8.2): ops/schema/<surface>.json validates the `ops`
// array of an apply_edits call. The files are byte-copies of the UI's compiled
// Zod schemas (MM108–MM110). A surface without a file is not offered
// apply_edits at all (decision 1).
//
//go:embed ops/schema
var opsFS embed.FS

// OpsValidator validates op lists per surface.
type OpsValidator struct {
	mu      sync.Mutex
	schemas map[string]*jsonschema.Schema
	raw     map[string][]byte
}

// NewOpsValidator loads every ops/schema/<surface>.json embedded in the build.
func NewOpsValidator() *OpsValidator {
	v := &OpsValidator{schemas: map[string]*jsonschema.Schema{}, raw: map[string][]byte{}}
	for _, s := range []string{SurfacePhoto, SurfaceStudio, SurfaceVector} {
		if b, err := fs.ReadFile(opsFS, "ops/schema/"+s+".json"); err == nil {
			v.raw[s] = compactJSON(b)
		}
	}
	return v
}

// compactJSON drops the file's indentation: the schema is inlined into the
// apply_edits tool definition, where every byte is an input token.
func compactJSON(b []byte) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return b
	}
	return out.Bytes()
}

// NewOpsValidatorFrom builds a validator from in-memory schemas (tests).
func NewOpsValidatorFrom(raw map[string][]byte) *OpsValidator {
	v := &OpsValidator{schemas: map[string]*jsonschema.Schema{}, raw: map[string][]byte{}}
	for k, b := range raw {
		v.raw[k] = b
	}
	return v
}

// Has reports whether the surface has an op schema (and so may edit).
func (v *OpsValidator) Has(surface string) bool {
	if v == nil {
		return false
	}
	_, ok := v.raw[surface]
	return ok
}

// Schema returns the raw schema of a surface (inlined into apply_edits).
func (v *OpsValidator) Schema(surface string) json.RawMessage {
	if v == nil {
		return nil
	}
	return v.raw[surface]
}

func (v *OpsValidator) compiled(surface string) (*jsonschema.Schema, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s, ok := v.schemas[surface]; ok {
		return s, nil
	}
	raw, ok := v.raw[surface]
	if !ok {
		return nil, fmt.Errorf("no op schema for surface %q", surface)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("op schema %s: %w", surface, err)
	}
	c := jsonschema.NewCompiler()
	url := "mem://ops/" + surface + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	s, err := c.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("op schema %s: %w", surface, err)
	}
	v.schemas[surface] = s
	return s, nil
}

// Validate checks an ops array (raw JSON) against the surface's schema.
func (v *OpsValidator) Validate(surface string, ops json.RawMessage) error {
	s, err := v.compiled(surface)
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(ops))
	if err != nil {
		return fmt.Errorf("ops are not JSON: %w", err)
	}
	return s.Validate(inst)
}

// validateArgs checks a tool call's arguments against a tool's parameter
// schema (strict: additionalProperties false on every tool).
func validateArgs(schema json.RawMessage, args string) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem://tool.json", doc); err != nil {
		return err
	}
	s, err := c.Compile("mem://tool.json")
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(args)))
	if err != nil {
		return fmt.Errorf("arguments are not JSON: %w", err)
	}
	return s.Validate(inst)
}
