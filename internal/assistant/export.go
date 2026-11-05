package assistant

// MM118: Medi recommends a Darkroom export, never starts one. A studio
// `suggest_action export` may carry params (preset, or container / videoCodec /
// audioCodec / audioBitrate / audioChannels, plus resolution and captionsMode).
// Before the action streams, the server validates them against the output
// table AND the caller's plan (the open_tool.options / ToolOptions.Validate
// precedent): an invalid value or combination goes back to the model as its
// retry; an above-plan choice is replaced by the nearest preset the plan
// allows, and the action carries `upgrade: {required, option}` so the dialog
// can show the plan that unlocks the original. The web export bridge opens the
// export dialog prefilled; only the user's click exports.
//
// ExportAdvisor is implemented in the handlers package over the output table
// (services) and the tier registry; it is pure and read-only.
type ExportAdvisor interface {
	// AdviseExport returns the normalized params for the caller's plan, or an
	// error for the model (unknown field, invalid value or combination).
	AdviseExport(tier string, params map[string]any) (map[string]any, error)
}

// SuggestKindExport is the suggest_action kind of a Darkroom export.
const SuggestKindExport = "export"
