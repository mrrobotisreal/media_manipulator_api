package services

// ParityExportArgs builds the exact ffmpeg invocation RunExport would issue for
// the plan — the same buildMultiTrackExportArgs filter graph — with a lossless
// libx264 encode substituted, so parity-harness frame diffs (part 13, ADR
// ws/0002) measure the filter math and the yuv420p 4:2:0 pipeline rather than
// encoder quantization. Used only by cmd/parity-render on the workstation.
func ParityExportArgs(plan StudioExportPlan, fontFile, fontsDir, outputPath string) []string {
	return buildMultiTrackExportArgs(plan, "libx264_lossless", "", fontFile, fontsDir, outputPath)
}
