package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// BurstConfig is the GPU burst configuration (MM124, code name `burst`,
// docs/design/GPU_BURST_EC2.md, ADR-0020). With nothing set burst is off:
// BURST_ENABLED defaults to false, and an enabled-but-incomplete setup behaves
// as disabled (fail closed, the ASSISTANT_ENABLED pattern). The reaper is the
// one exception: it runs whenever AWS credentials are present (AWSConfigured).
type BurstConfig struct {
	Enabled bool
	// Scope is BURST_SCOPE: "operator" (default) or "all".
	Scope string
	// Workloads is BURST_WORKLOADS (CSV of profile keys; empty = none).
	Workloads []string

	Region    string
	AccessKey string // BURST_AWS_ACCESS_KEY_ID; never log
	SecretKey string // BURST_AWS_SECRET_ACCESS_KEY; never log

	SubnetIDs       []string
	SecurityGroupID string
	InstanceProfile string
	// AMIs maps "family:arch" to an AMI id (BURST_AMIS).
	AMIs map[string]string
	// InstanceTypes maps an upper-case family to candidate overrides
	// (BURST_INSTANCE_TYPES_<FAMILY>).
	InstanceTypes map[string][]string
	// PriceUSDPerHour maps instance type to on-demand USD/hour. The defaults
	// are third-party figures, unverified until MM128.
	PriceUSDPerHour map[string]float64

	DailySpendCapUSD   float64
	MonthlySpendCapUSD float64

	// RefuseStaleImages (BURST_REFUSE_STALE_IMAGES, MM126): refuse to launch from
	// an image whose runtime manifest differs from the host's. Default false:
	// stale images still run, the drift is only logged and shown in burst-ctl.
	RefuseStaleImages bool
	// RootDevice (BURST_ROOT_DEVICE, MM126) is the AMI's root device name for the
	// volume-size override at launch: /dev/sda1 (default; what AWS reports for the
	// Deep Learning Base AMI, verified 2026-11-10) or /dev/xvda. The builder prints the real value.
	RootDevice string

	MaxInstances       int
	MaxInstanceMinutes int
	BootTimeoutSeconds int
	KeepFailedMinutes  int
	ReaperIntervalSecs int
	WorkerPort         int

	S3Prefix string

	TailscaleClientID     string
	TailscaleClientSecret string // never log
	TailscaleTailnet      string
	TailscaleTag          string

	TriggerDebounceSeconds  int
	MinPredictedWaitSeconds int

	// Cost-formula rates (design §8).
	EBSUSDPerGBMonth float64
	EgressUSDPerGB   float64
	VolumeInitUSD    float64
}

// DefaultBurstPrices are the design §5 figures (us-west-2, unverified).
var DefaultBurstPrices = map[string]float64{
	"g6.xlarge":  0.80,
	"g6e.xlarge": 1.86,
	"g7.2xlarge": 2.52,
}

// AWSConfigured reports whether the controller credentials are present (the
// reaper's condition, independent of Enabled).
func (b BurstConfig) AWSConfigured() bool {
	return strings.TrimSpace(b.AccessKey) != "" && strings.TrimSpace(b.SecretKey) != ""
}

// Missing lists what keeps an enabled burst from running ("" entries never).
func (b BurstConfig) Missing() []string {
	var m []string
	if !b.AWSConfigured() {
		m = append(m, "BURST_AWS_ACCESS_KEY_ID/BURST_AWS_SECRET_ACCESS_KEY")
	}
	if len(b.SubnetIDs) == 0 {
		m = append(m, "BURST_SUBNET_IDS")
	}
	if b.SecurityGroupID == "" {
		m = append(m, "BURST_SECURITY_GROUP_ID")
	}
	if b.InstanceProfile == "" {
		m = append(m, "BURST_INSTANCE_PROFILE")
	}
	if len(b.AMIs) == 0 {
		m = append(m, "BURST_AMIS")
	}
	if b.TailscaleClientID == "" || b.TailscaleClientSecret == "" || b.TailscaleTailnet == "" {
		m = append(m, "TAILSCALE_OAUTH_CLIENT_ID/TAILSCALE_OAUTH_CLIENT_SECRET/TAILSCALE_TAILNET")
	}
	return m
}

// Configured is enabled + everything needed to launch. Anything less behaves
// as disabled.
func (b BurstConfig) Configured() bool {
	return b.Enabled && len(b.Missing()) == 0
}

// WorkloadEnabled reports whether a profile key is switched on.
func (b BurstConfig) WorkloadEnabled(key string) bool {
	for _, w := range b.Workloads {
		if w == key {
			return true
		}
	}
	return false
}

// StartupLine is the one log line printed at boot.
func (b BurstConfig) StartupLine() string {
	state := "disabled"
	switch {
	case b.Enabled && b.Configured():
		state = "enabled"
	case b.Enabled:
		state = "disabled (enabled but not configured: missing " + strings.Join(b.Missing(), ", ") + ")"
	}
	types := make([]string, 0, len(b.PriceUSDPerHour))
	for t := range b.PriceUSDPerHour {
		types = append(types, t)
	}
	sort.Strings(types)
	return fmt.Sprintf("burst: %s scope=%s workloads=%s types=%s caps $%g/$%g reaper=%t",
		state, b.Scope, strings.Join(b.Workloads, ","), strings.Join(types, ","),
		b.DailySpendCapUSD, b.MonthlySpendCapUSD, b.AWSConfigured())
}

func loadBurstConfig() BurstConfig {
	scope := strings.ToLower(getEnv("BURST_SCOPE", "operator"))
	if scope != "all" {
		scope = "operator"
	}
	prices := map[string]float64{}
	for k, v := range DefaultBurstPrices {
		prices[k] = v
	}
	for k, v := range parseKVFloat(os.Getenv("BURST_PRICE_USD_PER_HOUR")) {
		prices[k] = v
	}
	types := map[string][]string{}
	for _, fam := range []string{"IMAGE", "AUDIO", "VIDEO", "RESTORE", "OLLAMA", "VOICES"} {
		if v := splitCSV(os.Getenv("BURST_INSTANCE_TYPES_" + fam)); len(v) > 0 {
			types[fam] = v
		}
	}
	return BurstConfig{
		Enabled:   getEnvBool("BURST_ENABLED", false),
		Scope:     scope,
		Workloads: splitCSV(os.Getenv("BURST_WORKLOADS")),

		Region:    getEnv("BURST_AWS_REGION", "us-west-2"),
		AccessKey: getEnv("BURST_AWS_ACCESS_KEY_ID", ""),
		SecretKey: getEnv("BURST_AWS_SECRET_ACCESS_KEY", ""),

		SubnetIDs:       splitCSV(os.Getenv("BURST_SUBNET_IDS")),
		SecurityGroupID: getEnv("BURST_SECURITY_GROUP_ID", ""),
		InstanceProfile: getEnv("BURST_INSTANCE_PROFILE", ""),
		AMIs:            parseKV(os.Getenv("BURST_AMIS")),
		InstanceTypes:   types,
		PriceUSDPerHour: prices,

		DailySpendCapUSD:   getEnvFloat("BURST_DAILY_SPEND_CAP_USD", 25),
		MonthlySpendCapUSD: getEnvFloat("BURST_MONTHLY_SPEND_CAP_USD", 250),

		RefuseStaleImages:  getEnvBool("BURST_REFUSE_STALE_IMAGES", false),
		RootDevice:         getEnv("BURST_ROOT_DEVICE", "/dev/sda1"),
		MaxInstances:       getEnvInt("BURST_MAX_INSTANCES", 2),
		MaxInstanceMinutes: getEnvInt("BURST_MAX_INSTANCE_MINUTES", 120),
		BootTimeoutSeconds: getEnvInt("BURST_BOOT_TIMEOUT_SECONDS", 600),
		KeepFailedMinutes:  getEnvIntDefault("BURST_KEEP_FAILED_MINUTES", 0),
		ReaperIntervalSecs: getEnvInt("BURST_REAPER_INTERVAL_SECONDS", 60),
		WorkerPort:         getEnvInt("BURST_WORKER_PORT", 7781),

		S3Prefix: getEnv("BURST_S3_PREFIX", "burst/"),

		TailscaleClientID:     getEnv("TAILSCALE_OAUTH_CLIENT_ID", ""),
		TailscaleClientSecret: getEnv("TAILSCALE_OAUTH_CLIENT_SECRET", ""),
		TailscaleTailnet:      getEnv("TAILSCALE_TAILNET", ""),
		TailscaleTag:          getEnv("BURST_TAILSCALE_TAG", "tag:mm-burst"),

		TriggerDebounceSeconds:  getEnvInt("BURST_TRIGGER_DEBOUNCE_SECONDS", 3),
		MinPredictedWaitSeconds: getEnvInt("BURST_MIN_PREDICTED_WAIT_SECONDS", 120),

		// EBS gp3 us-west-2 list price (unverified until MM128).
		EBSUSDPerGBMonth: getEnvFloat("BURST_EBS_USD_PER_GB_MONTH", 0.08),
		EgressUSDPerGB:   getEnvFloat("BURST_EGRESS_USD_PER_GB", 0.09),
		VolumeInitUSD:    0,
	}
}

// parseKV parses "a:b=c,d=e" into {"a:b":"c","d":"e"}.
func parseKV(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range splitCSV(raw) {
		i := strings.LastIndex(part, "=")
		if i <= 0 || i == len(part)-1 {
			continue
		}
		out[strings.TrimSpace(part[:i])] = strings.TrimSpace(part[i+1:])
	}
	return out
}

func parseKVFloat(raw string) map[string]float64 {
	out := map[string]float64{}
	for k, v := range parseKV(raw) {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			out[k] = f
		}
	}
	return out
}
