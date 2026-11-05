// Package flywheel is the logic behind cmd/assistant-flywheel (MM117, design
// MEDI_ASSISTANT.md §20): production signals become reviewable proposals.
//
// Pipeline (every step except Source and Labeler is pure and unit-tested):
//
//	Source.Messages   read-only SELECTs over assistant_messages + feedback (source.go)
//	BuildTurns        message rows -> turns (model.go)
//	Select            which turns carry a quality signal (signals.go)
//	Cluster           group by surface + retrieved/consulted doc ids (cluster.go)
//	Labeler           optional cheap Haiku label per cluster (label.go; costs money)
//	Propose           recipe stub / whenToUse sidecar fix / eval case YAML (propose.go)
//	Render            docs/evidence/medi-flywheel-<week>.md (report.go)
//	Scrub             PII removal applied to every quoted example (scrub.go)
//
// The package never writes to the database and never sends conversation text
// anywhere except, with --label, scrubbed <= 200-character examples to the
// title model.
package flywheel
