package lab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ParamsError is a client mistake in the params (a 400).
type ParamsError struct{ Msg string }

func (e *ParamsError) Error() string { return e.Msg }

// ValidateParams checks raw against the model's ParamsSchema and returns the
// params with every schema default filled in. raw may be nil or empty (all
// defaults). Unknown keys are refused when the schema says
// additionalProperties:false.
func ValidateParams(m LabModel, raw map[string]any) (map[string]any, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(m.ParamsSchema))
	if err != nil {
		return nil, fmt.Errorf("lab: schema of %s is not JSON: %w", m.Key, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem://params.json", doc); err != nil {
		return nil, err
	}
	sch, err := c.Compile("mem://params.json")
	if err != nil {
		return nil, fmt.Errorf("lab: schema of %s does not compile: %w", m.Key, err)
	}
	out := map[string]any{}
	for k, v := range raw {
		out[k] = v
	}
	var props struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(m.ParamsSchema, &props)
	names := make([]string, 0, len(props.Properties))
	for k := range props.Properties {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if _, has := out[k]; !has && props.Properties[k].Default != nil {
			out[k] = props.Properties[k].Default
		}
	}
	b, _ := json.Marshal(out)
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, &ParamsError{Msg: "params are not valid JSON"}
	}
	if err := sch.Validate(inst); err != nil {
		return nil, &ParamsError{Msg: "params do not match the model's schema: " + err.Error()}
	}
	// Re-decode so numbers are float64 like any JSON body.
	var final map[string]any
	_ = json.Unmarshal(b, &final)
	return final, nil
}
