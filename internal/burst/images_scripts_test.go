package burst

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/manifest"
)

func scriptsDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.Abs("../../scripts/burst")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The builder launches an AMI with the profile's root size; the AMI's snapshot
// must not be bigger, so lib.sh mirrors profiles.go and this keeps them equal.
func TestLibRootSizesMatchTheBurstProfiles(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(scriptsDir(t), "lib.sh"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s+(image|audio|video|restore\)?)\)? echo (\d+) ;;`)
	got := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		n, _ := strconv.Atoi(m[2])
		got[strings.TrimSuffix(m[1], ")")] = n
	}
	if len(got) != 4 {
		t.Fatalf("could not parse lib.sh root sizes: %v", got)
	}
	min := map[string]int{}
	for _, p := range profiles {
		if p.Never || p.Family == "" {
			continue
		}
		if m, ok := min[p.Family]; !ok || p.RootGiB < m {
			min[p.Family] = p.RootGiB
		}
	}
	for fam, gib := range got {
		if min[fam] != gib {
			t.Errorf("family %s: lib.sh says %d GiB, the smallest profile root is %d", fam, gib, min[fam])
		}
	}
}

func TestProvisionDryRunPrintsAPlanAndRunsNothing(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	for _, fam := range manifest.Families {
		out, err := exec.Command("bash", filepath.Join(scriptsDir(t), "provision-"+fam+".sh"), "--dry-run", "--date", "2026-11-10").CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", fam, err, out)
		}
		s := string(out)
		for _, want := range []string{"PLAN  ", "s3://media-manipulator/burst-images/2026-11-10/" + fam + ".tar.zst", "cloud-init clean", "smoke/" + fam + ".sh", "HF_HUB_OFFLINE=1"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s dry-run plan lacks %q", fam, want)
			}
		}
		hasMMCV := strings.Contains(s, "mmcv: rebuild")
		if (fam == "restore") != hasMMCV {
			t.Errorf("%s: mmcv rebuild presence wrong (%v)", fam, hasMMCV)
		}
		if strings.Contains(s, `8.9;12.0`) != true {
			t.Errorf("%s: plan must name both architectures", fam)
		}
		if strings.Contains(s, "magick ") && !strings.Contains(s, "never magick") {
			t.Errorf("%s: plan mentions magick", fam)
		}
	}
}

func TestBuildImageDryRunNeverCallsAWS(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	// A fake `aws` on PATH that fails loudly proves no call is made.
	bin := t.TempDir()
	fake := "#!/bin/sh\necho AWS-WAS-CALLED \"$@\" >&2\nexit 99\n"
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "restore", "2026-11-10", "--dry-run", "--yes")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "AWS-WAS-CALLED") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	for _, want := range []string{"Estimated cost", "mm-burst-build", "terminate", "BURST_AMIS=restore:x86_64="} {
		if !strings.Contains(string(out), want) {
			t.Errorf("dry-run missing %q", want)
		}
	}
	// Only g6.xlarge / g7.2xlarge are accepted (the IAM policy allows only those).
	bad := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "restore", "2026-11-10", "--dry-run", "--yes", "--instance-type", "p5.48xlarge")
	if out, err := bad.CombinedOutput(); err == nil {
		t.Fatalf("p5 must be refused:\n%s", out)
	}
}

// A real (non-dry) run up to the y/N prompt against a fake `aws` that answers
// like AWS did on 2026-11-10: the DLAMI's name has spaces in it, which once
// leaked into the root device name and broke every launch.
func TestBuildImageReadsRootDeviceFromANameWithSpaces(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	bin := t.TempDir()
	fake := `#!/bin/sh
case "$*" in
  *"s3api head-object"*) exit 0 ;;
  *"s3 cp s3://"*manifest.json*) echo '{"runtimeManifest":"abc123"}' ;;
  *"ssm get-parameter"*) echo ami-0e5b7b1a57b07ca51 ;;
  *"[Name,RootDeviceName]"*) printf 'Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20261106\t/dev/sda1\n' ;;
  *"Images[0].Name"*) printf 'Deep Learning Base OSS Nvidia Driver GPU AMI (Ubuntu 24.04) 20261106\n' ;;
  *"Images[0].RootDeviceName"*) echo /dev/sda1 ;;
  *"Ebs.VolumeSize"*) echo 75 ;;
  *"Images[0].Architecture"*) echo x86_64 ;;
  *) echo "UNEXPECTED: $*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "build-image.sh"), "video", "2026-11-10")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader("n\n") // decline at the launch prompt
	out, _ := cmd.CombinedOutput()
	got := string(out)
	if !strings.Contains(got, "root device /dev/sda1") {
		t.Fatalf("root device not parsed:\n%s", got)
	}
	if !strings.Contains(got, "Base root disk: 75 GiB") {
		t.Errorf("base root disk not read for /dev/sda1:\n%s", got)
	}
	if strings.Contains(got, "UNEXPECTED") || strings.Contains(got, "launched") {
		t.Errorf("must stop at the prompt without launching:\n%s", got)
	}
}

func TestExportDryRunListsPathsWithoutTouchingTheHost(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	// Stand-in burst-ctl: prints the real embedded lists.
	ctl := filepath.Join(t.TempDir(), "burst-ctl")
	script := "#!/bin/sh\ncd " + filepath.Join(scriptsDir(t), "../..") + " && exec go run ./cmd/burst-ctl \"$@\"\n"
	if err := os.WriteFile(ctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", filepath.Join(scriptsDir(t), "export-runtime.sh"), "restore", "--dry-run", "--date", "2026-11-10")
	cmd.Env = append(os.Environ(), "BURST_CTL="+ctl)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"s3://media-manipulator/burst-images/2026-11-10/restore.tar.zst", "/opt/media-manipulator-ai/venvs/restore-vsr-mm", "dry run"} {
		if !strings.Contains(s, want) {
			t.Errorf("export dry-run lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "/home/") || strings.Contains(s, ".env") && !strings.Contains(s, "Skipped everywhere") {
		t.Errorf("export plan lists a forbidden path:\n%s", s)
	}
}
