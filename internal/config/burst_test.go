package config

import (
	"strings"
	"testing"
)

func TestBurstConfig_DefaultsAreOff(t *testing.T) {
	b := loadBurstConfig()
	if b.Enabled || b.Configured() || b.AWSConfigured() {
		t.Fatal("zero env must be fully off")
	}
	if b.DailySpendCapUSD != 25 || b.MonthlySpendCapUSD != 250 || b.Region != "us-west-2" || b.Scope != "operator" ||
		b.MaxInstances != 2 || b.WorkerPort != 7781 || b.TailscaleTag != "tag:mm-burst" || b.S3Prefix != "burst/" {
		t.Fatalf("defaults wrong: %+v", b)
	}
	if b.PriceUSDPerHour["g6.xlarge"] != 0.80 {
		t.Fatal("default price missing")
	}
}

// Config fail-closed: enabled but anything missing is not Configured.
func TestBurstConfig_FailClosed(t *testing.T) {
	t.Setenv("BURST_ENABLED", "true")
	if loadBurstConfig().Configured() {
		t.Fatal("enabled without AWS/Tailscale must not be configured")
	}
	line := loadBurstConfig().StartupLine()
	if !strings.Contains(line, "disabled") || !strings.Contains(line, "BURST_SUBNET_IDS") {
		t.Fatalf("startup line = %q", line)
	}
	t.Setenv("BURST_AWS_ACCESS_KEY_ID", "AKIAFAKE")
	t.Setenv("BURST_AWS_SECRET_ACCESS_KEY", "s3cret")
	t.Setenv("BURST_SUBNET_IDS", "subnet-a, subnet-b")
	t.Setenv("BURST_SECURITY_GROUP_ID", "sg-1")
	t.Setenv("BURST_INSTANCE_PROFILE", "mm-burst-worker")
	t.Setenv("BURST_AMIS", "image:x86_64=ami-1")
	if loadBurstConfig().Configured() {
		t.Fatal("still missing Tailscale")
	}
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "id")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "sec")
	t.Setenv("TAILSCALE_TAILNET", "example.ts.net")
	b := loadBurstConfig()
	if !b.Configured() || len(b.SubnetIDs) != 2 || b.AMIs["image:x86_64"] != "ami-1" {
		t.Fatalf("should be configured: %+v", b)
	}
	if strings.Contains(b.StartupLine(), "s3cret") || strings.Contains(b.StartupLine(), "AKIAFAKE") {
		t.Fatal("startup line leaks a credential")
	}
}

func TestBurstConfig_ReaperRunsWithoutEnabled(t *testing.T) {
	t.Setenv("BURST_AWS_ACCESS_KEY_ID", "a")
	t.Setenv("BURST_AWS_SECRET_ACCESS_KEY", "b")
	b := loadBurstConfig()
	if b.Enabled || !b.AWSConfigured() {
		t.Fatal("creds alone: reaper condition true, burst off")
	}
}

func TestBurstConfig_Overrides(t *testing.T) {
	t.Setenv("BURST_PRICE_USD_PER_HOUR", "g6.xlarge=0.9,x.large=1.5,bad=zz")
	t.Setenv("BURST_INSTANCE_TYPES_IMAGE", "g6e.xlarge,g6.xlarge")
	t.Setenv("BURST_WORKLOADS", "remove_background, ai_upscale")
	t.Setenv("BURST_SCOPE", "ALL")
	b := loadBurstConfig()
	if b.PriceUSDPerHour["g6.xlarge"] != 0.9 || b.PriceUSDPerHour["x.large"] != 1.5 || b.PriceUSDPerHour["g7.2xlarge"] != 2.52 {
		t.Fatalf("prices: %v", b.PriceUSDPerHour)
	}
	if _, bad := b.PriceUSDPerHour["bad"]; bad {
		t.Fatal("bad price accepted")
	}
	if b.InstanceTypes["IMAGE"][0] != "g6e.xlarge" || !b.WorkloadEnabled("ai_upscale") || b.WorkloadEnabled("demucs") || b.Scope != "all" {
		t.Fatalf("overrides: %+v", b)
	}
}
