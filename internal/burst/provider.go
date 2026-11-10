package burst

import (
	"context"
	"errors"
	"time"
)

// Typed provider errors (design §5, §8).
var (
	// ErrNoCapacity: InsufficientInstanceCapacity / Unsupported. Try the next
	// candidate and subnet.
	ErrNoCapacity = errors.New("burst: no capacity")
	// ErrQuota: VcpuLimitExceeded / InstanceLimitExceeded.
	ErrQuota = errors.New("burst: quota exceeded")
	// ErrDenied: UnauthorizedOperation. Also what the AWS Budget action looks
	// like once it fires. Never retried.
	ErrDenied = errors.New("burst: denied by AWS")
)

// LaunchRequest is one launch. Candidates x Subnets are tried in order and the
// first success wins.
type LaunchRequest struct {
	RunID      string // also the idempotency ClientToken
	JobID      string
	Workload   string
	Candidates []string
	Subnets    []string
	// AMI by instance type, resolved by the caller from the arch of the type.
	AMIFor          func(instanceType string) string
	SecurityGroupID string
	InstanceProfile string
	RootGiB         int
	RootDevice      string // default /dev/xvda
	UserData        string // opaque, plain text; the provider base64-encodes it
}

// Instance is the provider's view of one instance.
type Instance struct {
	ID         string
	Type       string
	AZ         string
	AMI        string
	State      string // pending, running, stopping, stopped, shutting-down, terminated
	LaunchTime time.Time
	RunID      string // tag mm-burst-run
	JobID      string // tag mm-burst-job
	Build      string // tag mm-burst-build (MM126 image builder, not a run)
}

// Provider is the cloud seam; tests never touch AWS.
type Provider interface {
	Launch(ctx context.Context, req LaunchRequest) (Instance, error)
	Describe(ctx context.Context, ids []string) ([]Instance, error)
	// ListTagged returns instances tagged mm-burst=1 in pending, running or
	// stopping state.
	ListTagged(ctx context.Context) ([]Instance, error)
	Terminate(ctx context.Context, ids []string) error
}

// Tag keys (also the IAM condition keys, design §8).
const (
	TagBurst = "mm-burst"
	TagRun   = "mm-burst-run"
	TagJob   = "mm-burst-job"
	// TagBuild marks an AMI-builder instance (MM126). It also carries
	// mm-burst=1 so it counts against the burst budget, but it is not a run:
	// the reaper leaves it alone until it is older than BuildMaxAge.
	TagBuild = "mm-burst-build"
)
