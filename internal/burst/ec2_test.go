package burst

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

type fakeEC2 struct {
	mu        sync.Mutex
	runs      []*ec2.RunInstancesInput
	failures  map[string]error // key "type|subnet"
	described []*ec2.DescribeInstancesInput
	terminate [][]string
	listOut   []types.Instance
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, in)
	if err := f.failures[string(in.InstanceType)+"|"+aws.ToString(in.SubnetId)]; err != nil {
		return nil, err
	}
	return &ec2.RunInstancesOutput{Instances: []types.Instance{{
		InstanceId: aws.String("i-123"), InstanceType: in.InstanceType, ImageId: in.ImageId,
		State:     &types.InstanceState{Name: types.InstanceStateNamePending},
		Placement: &types.Placement{AvailabilityZone: aws.String("us-west-2b")},
	}}}, nil
}
func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.described = append(f.described, in)
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: f.listOut}}}, nil
}
func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminate = append(f.terminate, in.InstanceIds)
	return &ec2.TerminateInstancesOutput{}, nil
}

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: "x"} }

func testReq() LaunchRequest {
	return LaunchRequest{
		RunID: "0b1f6e0e-0000-4000-8000-000000000001", JobID: "job-9", Workload: "remove_background",
		Candidates: []string{"g6.xlarge", "g7.2xlarge"}, Subnets: []string{"subnet-a", "subnet-b"},
		AMIFor:          func(string) string { return "ami-abc" },
		SecurityGroupID: "sg-1", InstanceProfile: "mm-burst-worker", RootGiB: 120, UserData: "#!/bin/bash\nshutdown -h +5\n",
	}
}

// On-demand request shape: no market options, terminate-on-shutdown, IMDSv2,
// tags on instance and volume, encrypted gp3, client token.
func TestEC2_OnDemandRequestShape(t *testing.T) {
	f := &fakeEC2{}
	p := NewEC2Provider(f, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	inst, err := p.Launch(context.Background(), testReq())
	if err != nil || inst.ID != "i-123" || inst.AZ != "us-west-2b" {
		t.Fatalf("launch: %+v %v", inst, err)
	}
	if len(f.runs) != 1 {
		t.Fatalf("first success must stop iteration, got %d calls", len(f.runs))
	}
	in := f.runs[0]
	if in.InstanceMarketOptions != nil {
		t.Fatal("InstanceMarketOptions must be nil: on-demand only, never spot")
	}
	if in.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorTerminate {
		t.Error("shutdown behavior must be terminate")
	}
	if in.MetadataOptions == nil || in.MetadataOptions.HttpTokens != types.HttpTokensStateRequired || aws.ToInt32(in.MetadataOptions.HttpPutResponseHopLimit) != 1 {
		t.Errorf("IMDSv2 required with hop limit 1: %+v", in.MetadataOptions)
	}
	if aws.ToInt32(in.MinCount) != 1 || aws.ToInt32(in.MaxCount) != 1 {
		t.Error("min=max=1")
	}
	if aws.ToString(in.ClientToken) != testReq().RunID {
		t.Error("client token must be the run id")
	}
	if len(in.BlockDeviceMappings) != 1 {
		t.Fatal("one root mapping")
	}
	ebs := in.BlockDeviceMappings[0].Ebs
	if ebs.VolumeType != types.VolumeTypeGp3 || !aws.ToBool(ebs.Encrypted) || !aws.ToBool(ebs.DeleteOnTermination) || aws.ToInt32(ebs.VolumeSize) != 120 {
		t.Errorf("encrypted gp3, delete on termination, sized from profile: %+v", ebs)
	}
	if len(in.TagSpecifications) != 2 {
		t.Fatal("tags on instance and volume")
	}
	for _, ts := range in.TagSpecifications {
		got := map[string]string{}
		for _, tg := range ts.Tags {
			got[aws.ToString(tg.Key)] = aws.ToString(tg.Value)
		}
		if got["mm-burst"] != "1" || got["mm-burst-run"] != testReq().RunID || got["mm-burst-job"] != "job-9" || got["Name"] != "mm-burst-remove_background" {
			t.Errorf("%s tags: %v", ts.ResourceType, got)
		}
	}
	if in.TagSpecifications[0].ResourceType != types.ResourceTypeInstance || in.TagSpecifications[1].ResourceType != types.ResourceTypeVolume {
		t.Error("tag spec resource types")
	}
	if aws.ToString(in.IamInstanceProfile.Name) != "mm-burst-worker" || in.SecurityGroupIds[0] != "sg-1" || aws.ToString(in.SubnetId) != "subnet-a" {
		t.Error("profile / sg / subnet")
	}
	ud, _ := base64.StdEncoding.DecodeString(aws.ToString(in.UserData))
	if !strings.HasPrefix(string(ud), "#!/bin/bash") {
		t.Error("user data must be base64 of the opaque string")
	}
	// Print the captured input (acceptance demonstration; run with -v).
	b, _ := json.MarshalIndent(in, "", "  ")
	t.Logf("RunInstancesInput:\n%s", b)
}

func TestEC2_FallbackAcrossCandidatesAndSubnets(t *testing.T) {
	f := &fakeEC2{failures: map[string]error{
		"g6.xlarge|subnet-a":  apiErr("InsufficientInstanceCapacity"),
		"g6.xlarge|subnet-b":  apiErr("Unsupported"),
		"g7.2xlarge|subnet-a": apiErr("VcpuLimitExceeded"),
	}}
	p := NewEC2Provider(f, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	inst, err := p.Launch(context.Background(), testReq())
	if err != nil || inst.Type != "g7.2xlarge" {
		t.Fatalf("want g7.2xlarge in subnet-b: %+v %v", inst, err)
	}
	var order []string
	for _, in := range f.runs {
		order = append(order, fmt.Sprintf("%s|%s", in.InstanceType, aws.ToString(in.SubnetId)))
	}
	if strings.Join(order, ",") != "g6.xlarge|subnet-a,g6.xlarge|subnet-b,g7.2xlarge|subnet-a,g7.2xlarge|subnet-b" {
		t.Fatalf("order = %v", order)
	}
}

func TestEC2_AllCandidatesFailIsNoCapacity(t *testing.T) {
	f := &fakeEC2{failures: map[string]error{}}
	for _, ty := range []string{"g6.xlarge", "g7.2xlarge"} {
		for _, s := range []string{"subnet-a", "subnet-b"} {
			f.failures[ty+"|"+s] = apiErr("InsufficientInstanceCapacity")
		}
	}
	_, err := NewEC2Provider(f, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))).Launch(context.Background(), testReq())
	if !errors.Is(err, ErrNoCapacity) || len(f.runs) != 4 {
		t.Fatalf("err=%v calls=%d", err, len(f.runs))
	}
}

func TestEC2_QuotaMapsToErrQuota(t *testing.T) {
	f := &fakeEC2{failures: map[string]error{}}
	for _, ty := range []string{"g6.xlarge", "g7.2xlarge"} {
		for _, s := range []string{"subnet-a", "subnet-b"} {
			f.failures[ty+"|"+s] = apiErr("InstanceLimitExceeded")
		}
	}
	_, err := NewEC2Provider(f, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))).Launch(context.Background(), testReq())
	if !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
}

// UnauthorizedOperation is ErrDenied and is never retried.
func TestEC2_UnauthorizedIsDeniedNoRetry(t *testing.T) {
	f := &fakeEC2{failures: map[string]error{"g6.xlarge|subnet-a": apiErr("UnauthorizedOperation")}}
	_, err := NewEC2Provider(f, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))).Launch(context.Background(), testReq())
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("want ErrDenied: %v", err)
	}
	if len(f.runs) != 1 {
		t.Fatalf("denied must not retry: %d calls", len(f.runs))
	}
}

func TestEC2_ListTaggedFiltersAndTerminate(t *testing.T) {
	f := &fakeEC2{listOut: []types.Instance{{InstanceId: aws.String("i-1"), Tags: []types.Tag{{Key: aws.String(TagRun), Value: aws.String("r1")}}}}}
	p := NewEC2Provider(f, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	got, err := p.ListTagged(context.Background())
	if err != nil || len(got) != 1 || got[0].RunID != "r1" {
		t.Fatalf("%v %v", got, err)
	}
	var names []string
	for _, flt := range f.described[0].Filters {
		names = append(names, aws.ToString(flt.Name)+"="+strings.Join(flt.Values, "/"))
	}
	if strings.Join(names, ";") != "tag:mm-burst=1;instance-state-name=pending/running/stopping" {
		t.Fatalf("filters: %v", names)
	}
	if err := p.Terminate(context.Background(), []string{"i-1"}); err != nil || len(f.terminate) != 1 {
		t.Fatal("terminate")
	}
}

// Minter request body, token caching, and no key in the captured log.
func TestTailscaleMinter_BodyCacheAndNoKeyInLogs(t *testing.T) {
	const secretKey = "tskey-auth-SUPERSECRETKEY123"
	const clientSecret = "tskey-client-OAUTHSECRET"
	var tokenCalls, keyCalls int
	var gotBody KeyRequest
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/oauth/token":
			tokenCalls++
			_ = r.ParseForm()
			if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != clientSecret {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"atok","token_type":"Bearer","expires_in":3600}`))
		default:
			keyCalls++
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = w.Write([]byte(`{"id":"k1","key":"` + secretKey + `"}`))
		}
	}))
	defer srv.Close()
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	m := &TailscaleMinter{BaseURL: srv.URL, ClientID: "cid", ClientSecret: clientSecret, Tailnet: "example.ts.net", Tag: "tag:mm-burst", Log: log}
	k, err := m.Mint(context.Background())
	if err != nil || k != secretKey {
		t.Fatalf("mint: %q %v", k, err)
	}
	if _, err := m.Mint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 1 || keyCalls != 2 {
		t.Fatalf("token must be cached: token=%d key=%d", tokenCalls, keyCalls)
	}
	c := gotBody.Capabilities.Devices.Create
	if c.Reusable || !c.Ephemeral || !c.Preauthorized || len(c.Tags) != 1 || c.Tags[0] != "tag:mm-burst" || gotBody.ExpirySeconds != 600 {
		t.Fatalf("body: %+v", gotBody)
	}
	if gotAuth != "Bearer atok" || gotPath != "/api/v2/tailnet/example.ts.net/keys" {
		t.Fatalf("auth=%q path=%q", gotAuth, gotPath)
	}
	if strings.Contains(logBuf.String(), secretKey) || strings.Contains(logBuf.String(), clientSecret) || strings.Contains(logBuf.String(), "atok") {
		t.Fatalf("a secret reached the log:\n%s", logBuf.String())
	}
	if logBuf.Len() == 0 {
		t.Fatal("expected a log line")
	}
}

func TestTailscaleMinter_ErrorsHaveNoSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv.Close()
	m := &TailscaleMinter{BaseURL: srv.URL, ClientID: "cid", ClientSecret: "topsecret", Tailnet: "t", Tag: "tag:x"}
	_, err := m.Mint(context.Background())
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("err = %v", err)
	}
}
