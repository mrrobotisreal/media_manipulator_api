package models

// JobFinished is what the job manager hands the finished-job notifiers (MA23):
// a long, unwatched AI job. It carries ids and the tool id only, never a file name.
type JobFinished struct {
	UserID string
	JobID  string
	Tool   string // JobFacts.Tool, e.g. "ai_upscale"
	Failed bool
	// VisitorID / SessionID are the creating request's correlation ids, used for
	// the server_push_sent identity (empty = the emitter drops the event).
	VisitorID string
	SessionID string
}
