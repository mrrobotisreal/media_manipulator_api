package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/pricing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

var now0 = time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC)

type memStore struct{ runs []burst.Run }

func (m *memStore) SettledMicrosSince(_ context.Context, since time.Time) (int64, error) {
	var n int64
	for _, r := range m.runs {
		if !r.OfferedAt.Before(since) {
			n += r.CostMicros
		}
	}
	return n, nil
}
func (m *memStore) Insert(context.Context, *burst.Run) error              { return nil }
func (m *memStore) Update(context.Context, *burst.Run) error              { return nil }
func (m *memStore) ListUnterminated(context.Context) ([]burst.Run, error) { return nil, nil }
func (m *memStore) ListSince(_ context.Context, since time.Time) ([]burst.Run, error) {
	var out []burst.Run
	for _, r := range m.runs {
		if !r.OfferedAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

type memProv struct {
	tagged     []burst.Instance
	terminated []string
}

func (m *memProv) Launch(context.Context, burst.LaunchRequest) (burst.Instance, error) {
	panic("status/reap tests never launch")
}
func (m *memProv) Describe(context.Context, []string) ([]burst.Instance, error) { return nil, nil }
func (m *memProv) ListTagged(context.Context) ([]burst.Instance, error)         { return m.tagged, nil }
func (m *memProv) Terminate(_ context.Context, ids []string) error {
	m.terminated = append(m.terminated, ids...)
	return nil
}

func testEnv(buf *bytes.Buffer) (*env, *memProv) {
	cfg := config.BurstConfig{Enabled: true, Scope: "operator", Workloads: []string{"remove_background"},
		DailySpendCapUSD: 25, MonthlySpendCapUSD: 250, MaxInstanceMinutes: 120, Region: "us-west-2",
		AccessKey: "a", SecretKey: "b", EBSUSDPerGBMonth: 0.08,
		PriceUSDPerHour: map[string]float64{"g6.xlarge": 0.8, "g7.2xlarge": 2.52}}
	st := &memStore{runs: []burst.Run{
		{ID: "11111111-aaaa", Workload: "remove_background", InstanceType: "g6.xlarge", State: burst.StateCompleted, OfferedAt: now0.Add(-3 * time.Hour), CostMicros: 41_000},
		{ID: "22222222-bbbb", Workload: "demucs", InstanceType: "g7.2xlarge", State: burst.StateLostRace, OfferedAt: now0.Add(-1 * time.Hour), CostMicros: 21_000},
		{ID: "33333333-cccc", Workload: "demucs", InstanceType: "g7.2xlarge", State: burst.StateCompleted, OfferedAt: now0.AddDate(0, 0, -2), CostMicros: 3_500_000},
	}}
	prov := &memProv{tagged: []burst.Instance{{ID: "i-0abc", Type: "g6.xlarge", AZ: "us-west-2b", State: "running", RunID: "44444444-dddd", LaunchTime: now0.Add(-7 * time.Minute)}}}
	return &env{cfg: cfg, provider: prov, store: st, now: func() time.Time { return now0 }, out: buf}, prov
}

// A sample `burst-ctl status` from a fake store (acceptance demonstration).
func TestStatusOutput(t *testing.T) {
	var buf bytes.Buffer
	e, _ := testEnv(&buf)
	if err := e.status(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	t.Logf("\n%s", out)
	for _, want := range []string{"live instances: 1", "i-0abc", "g6.xlarge", "age=7m0s",
		"today (UTC):      $0.06 of $25.00", "this month (UTC): $3.56 of $250.00", "caps $25/$250"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q", want)
		}
	}
}

func TestRunsOutput(t *testing.T) {
	var buf bytes.Buffer
	e, _ := testEnv(&buf)
	if err := e.runs(context.Background(), []string{"--since", "24h"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "11111111") || !strings.Contains(out, "lost_race") || strings.Contains(out, "33333333") {
		t.Fatalf("runs --since 24h:\n%s", out)
	}
}

func TestReapDryRunByDefault(t *testing.T) {
	var buf bytes.Buffer
	e, prov := testEnv(&buf)
	if err := e.reap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(prov.terminated) != 0 || !strings.Contains(buf.String(), "DRY RUN") || !strings.Contains(buf.String(), "terminate i-0abc") {
		t.Fatalf("dry run:\n%s", buf.String())
	}
	buf.Reset()
	if err := e.reap(context.Background(), []string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	if len(prov.terminated) != 1 || prov.terminated[0] != "i-0abc" {
		t.Fatalf("--yes must terminate: %v", prov.terminated)
	}
}

func TestLaunchTestNeedsExplicitFlag(t *testing.T) {
	var buf bytes.Buffer
	e, _ := testEnv(&buf)
	err := e.launchTest(context.Background(), []string{"--workload", "remove_background"})
	if err == nil || !strings.Contains(err.Error(), "--i-know-this-costs-money") {
		t.Fatalf("err = %v", err)
	}
	if err := e.launchTest(context.Background(), []string{"--workload", "rembg[photo]", "--i-know-this-costs-money"}); err == nil {
		t.Fatal("a never workload must be refused")
	}
}

type fakePricing struct{}

func (fakePricing) GetProducts(_ context.Context, in *pricing.GetProductsInput, _ ...func(*pricing.Options)) (*pricing.GetProductsOutput, error) {
	return &pricing.GetProductsOutput{PriceList: []string{
		`{"terms":{"OnDemand":{"SKU.TERM":{"priceDimensions":{"SKU.TERM.RATE":{"unit":"Hrs","pricePerUnit":{"USD":"0.8048000000"}}}}}}}`}}, nil
}

func TestPricesCompareToConfig(t *testing.T) {
	var buf bytes.Buffer
	e, _ := testEnv(&buf)
	if err := e.printPrices(context.Background(), fakePricing{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "g6.xlarge") || !strings.Contains(buf.String(), "0.8048") || !strings.Contains(buf.String(), "+0.0048") {
		t.Fatalf("prices:\n%s", buf.String())
	}
}

// launchProv launches and reports running at once.
type launchProv struct {
	memProv
	req burst.LaunchRequest
}

func (p *launchProv) Launch(_ context.Context, r burst.LaunchRequest) (burst.Instance, error) {
	p.req = r
	return burst.Instance{ID: "i-0lab", Type: r.Candidates[0], AMI: r.AMIFor(r.Candidates[0]), State: "running"}, nil
}
func (p *launchProv) Describe(context.Context, []string) ([]burst.Instance, error) {
	return []burst.Instance{{ID: "i-0lab", State: "running"}}, nil
}

// MM139: the first live burst launch is tested with a Lab workload, so launch-test
// accepts a Lab tool name and launches the lab-voice image through LaunchDirect.
func TestLaunchTestAcceptsALabWorkload(t *testing.T) {
	var buf bytes.Buffer
	e, _ := testEnv(&buf)
	prov := &launchProv{}
	e.provider = prov
	e.cfg.MaxInstances = 2
	e.cfg.AMIs = map[string]string{"lab-voice:x86_64": "ami-voice"}
	e.cfg.SubnetIDs = []string{"subnet-1"}
	e.cfg.SecurityGroupID, e.cfg.InstanceProfile = "sg-1", "mm-burst-worker"
	if err := e.launchTest(context.Background(), []string{"--workload", "lab_fish_s2", "--i-know-this-costs-money", "--wait", "1s"}); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if len(prov.req.Candidates) != 1 || prov.req.Candidates[0] != "g6e.xlarge" || prov.req.AMIFor("g6e.xlarge") != "ami-voice" {
		t.Fatalf("launched %+v", prov.req)
	}
	if !strings.Contains(buf.String(), "launched i-0lab") || !strings.Contains(buf.String(), "terminated; cost") {
		t.Fatalf("output:\n%s", buf.String())
	}
	// A product workload name still goes through the product table, and an unknown lab name is refused.
	if err := e.launchTest(context.Background(), []string{"--workload", "lab_nope", "--i-know-this-costs-money"}); err == nil {
		t.Fatal("an unknown lab workload must be refused")
	}
}
