package burst

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Run purposes (burst_runs.purpose).
const (
	PurposeBurst = "burst"
	PurposeLab   = "lab"
)

// State is burst_runs.state (design §6).
type State string

const (
	StateOffered    State = "offered"
	StateLaunching  State = "launching"
	StateBooting    State = "booting"
	StateReady      State = "ready"
	StateStagingIn  State = "staging_in"
	StateRunning    State = "running"
	StateStagingOut State = "staging_out"
	StateCompleted  State = "completed"

	StateLostRace     State = "lost_race"
	StateAdopted      State = "adopted"
	StateLaunchFailed State = "launch_failed"
	StateBootTimeout  State = "boot_timeout"
	StateWorkerLost   State = "worker_lost"
	StateToolFailed   State = "tool_failed"
	StateCapRefused   State = "cap_refused"
	StateOrphaned     State = "orphaned"
)

// ErrIllegalTransition is returned for an edge not in the table.
var ErrIllegalTransition = errors.New("burst: illegal state transition")

// Terminal reports whether no further transition is allowed.
func (s State) Terminal() bool {
	switch s {
	case StateCompleted, StateLostRace, StateAdopted, StateLaunchFailed, StateBootTimeout,
		StateWorkerLost, StateToolFailed, StateCapRefused, StateOrphaned:
		return true
	}
	return false
}

// exits are reachable from every live state: the side exits.
var exits = []State{StateLostRace, StateAdopted, StateLaunchFailed, StateBootTimeout,
	StateWorkerLost, StateToolFailed, StateOrphaned}

// transitions is the enforced edge table. offered may be refused by the cap;
// the happy path is a straight line.
var transitions = map[State][]State{
	StateOffered:   {StateLaunching, StateCapRefused},
	StateLaunching: {StateBooting},
	StateBooting:   {StateReady},
	StateReady:     {StateStagingIn},
	StateStagingIn: {StateRunning},
	StateRunning:   {StateStagingOut},
	// staging_out -> staging_in is the Lab session re-arm (MM138): a parked
	// instance takes its next job without going through ready again.
	StateStagingOut: {StateCompleted, StateStagingIn},
}

// CanTransition reports whether from -> to is legal.
func CanTransition(from, to State) bool {
	if from.Terminal() {
		return false
	}
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	if from == StateOffered {
		// An offered run can lose the race or be orphaned before it launches.
		return to == StateLostRace || to == StateOrphaned || to == StateLaunchFailed
	}
	for _, e := range exits {
		if e == to {
			return true
		}
	}
	return false
}

// Run is one burst_runs row.
type Run struct {
	ID              string
	JobID           string
	Workload        string
	Kind            string
	Family          string
	Arch            string
	InstanceType    string
	InstanceID      string
	AZ              string
	AMIID           string
	State           State
	Tier            string
	Priority        int
	PredictedWaitMs int64
	OfferedAt       time.Time
	LaunchedAt      *time.Time
	ReadyAt         *time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	TerminatedAt    *time.Time
	BytesIn         int64
	BytesOut        int64
	ReservedMicros  int64
	CostMicros      int64
	ErrorType       string
	AdoptedFrom     string
	// Purpose is burst_runs.purpose: PurposeBurst (default) or PurposeLab (MM138).
	Purpose string
	// SessionUntil is burst_runs.session_until: while a Lab session instance is
	// parked it is the time the instance is kept until; nil = not parked.
	SessionUntil *time.Time
	// Keep (not persisted) asks Remote.Run to leave the instance running after
	// a successful run, for the next job of a Lab session. A failed run still
	// terminates.
	Keep bool
	// WorkerID is the run id baked into the instance (its tailnet hostname, run
	// token, staging prefix and data dir). It equals ID except on an adopted
	// run (MM127): the new row gets a new ID but the instance still answers to
	// the run it was launched for. Not persisted; empty means ID.
	WorkerID string
}

// WorkerRunID is the identity the worker on this run's instance knows.
func (r Run) WorkerRunID() string {
	if r.WorkerID != "" {
		return r.WorkerID
	}
	return r.ID
}

// Transition moves the run along an edge of the table, stamping the matching
// timestamp. An illegal edge returns ErrIllegalTransition and logs.
func (r *Run) Transition(to State, now time.Time, log *slog.Logger) error {
	if !CanTransition(r.State, to) {
		if log != nil {
			log.Error("burst: illegal state transition", "run", r.ID, "from", string(r.State), "to", string(to))
		}
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, r.State, to)
	}
	r.State = to
	t := now
	switch to {
	case StateLaunching:
		r.LaunchedAt = &t
	case StateReady:
		r.ReadyAt = &t
	case StateRunning:
		r.StartedAt = &t
	case StateStagingOut, StateCompleted:
		if to == StateCompleted || r.FinishedAt == nil {
			r.FinishedAt = &t
		}
	}
	return nil
}
