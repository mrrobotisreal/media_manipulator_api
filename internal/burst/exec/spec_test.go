package exec

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

var testRoots = RootsFrom("/srv/uploads", "/srv/outputs", "/srv/temp", "/srv/uploads/photo")

func TestValidate_UndeclaredPathRejected(t *testing.T) {
	v := Validator{Roots: testRoots}
	ok := Spec{
		Steps:   []Cmd{{Exe: "/opt/media-manipulator-ai/bin/demucs", Args: []string{"-o", "/srv/outputs/j1/stems", "/srv/uploads/j1/in.wav"}, Env: []string{"HF_HOME=/var/lib/creatv/hf", "X=/opt/media-manipulator-ai/models/m"}}},
		Inputs:  []Path{P("/srv/uploads/j1/in.wav")},
		Outputs: []Path{D("/srv/outputs/j1/stems")},
	}
	if err := v.Validate(ok); err != nil {
		t.Fatalf("declared spec rejected: %v", err)
	}
	cases := map[string]Cmd{
		"arg":       {Exe: "x", Args: []string{"/srv/uploads/j2/other.wav"}},
		"env":       {Exe: "x", Env: []string{"OUT=/srv/temp/leak"}},
		"dir":       {Exe: "x", Dir: "/srv/temp"},
		"embedded":  {Exe: "x", Args: []string{"--in=/srv/outputs/j9/y"}},
		"photo":     {Exe: "x", Args: []string{"/srv/uploads/photo/p1/a.png"}},
		"aitmp":     {Exe: "x", Args: []string{"/opt/media-manipulator-ai/tmp/z"}},
		"unknownai": {Exe: "x", Args: []string{"/opt/media-manipulator-ai/secrets/k"}},
		"shellcmd":  {Exe: "bash", Args: []string{"-lc", "cat /srv/uploads/j2/a > /srv/outputs/j1/stems/b"}},
	}
	for name, c := range cases {
		s := ok
		s.Steps = []Cmd{c}
		if err := v.Validate(s); !errors.Is(err, ErrUndeclaredPath) {
			t.Errorf("%s: want ErrUndeclaredPath, got %v", name, err)
		}
	}
	// Sibling with a common name prefix is not covered by a declaration.
	s := ok
	s.Steps = []Cmd{{Exe: "x", Args: []string{"/srv/uploads/j1/in.wav.bak"}}}
	if err := v.Validate(s); !errors.Is(err, ErrUndeclaredPath) {
		t.Errorf("name-prefix sibling accepted: %v", err)
	}
	// The error never carries more than the root.
	if err := v.Validate(Spec{}); err == nil {
		t.Error("empty steps accepted")
	}
}

func TestValidate_BadDeclarations(t *testing.T) {
	v := Validator{Roots: testRoots}
	for name, s := range map[string]Spec{
		"relative":  {Steps: []Cmd{{Exe: "x"}}, Inputs: []Path{P("rel/in")}},
		"duplicate": {Steps: []Cmd{{Exe: "x"}}, Inputs: []Path{P("/srv/uploads/a")}, Scratch: []Path{D("/srv/uploads/a")}},
		"unclean":   {Steps: []Cmd{{Exe: "x"}}, Inputs: []Path{P("/srv/uploads/../a")}},
	} {
		if err := v.Validate(s); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func mapperFor(t *testing.T, s Spec) *Mapper {
	t.Helper()
	m, err := NewMapper("", "run-1", s)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMapper_LongestPrefixAndNothingElseTouched(t *testing.T) {
	s := Spec{
		Steps:   []Cmd{{Exe: "x"}},
		Inputs:  []Path{D("/srv/uploads/j1"), P("/srv/uploads/j1/special.wav")},
		Outputs: []Path{D("/srv/outputs/j1")},
		Scratch: []Path{D("/srv/temp/j1")},
	}
	m := mapperFor(t, s)
	base := "/opt/dlami/nvme/burst/run-1"
	cases := map[string]string{
		"/srv/uploads/j1/special.wav":            base + "/in/1/special.wav", // longest wins over the dir
		"/srv/uploads/j1/other.wav":              base + "/in/0/j1/other.wav",
		"--o=/srv/outputs/j1/x":                  "--o=" + base + "/out/0/j1/x",
		"A=/srv/temp/j1,B=/srv/uploads/j1":       "A=" + base + "/scratch/0/j1,B=" + base + "/in/0/j1",
		"/srv/uploads/j10/a":                     "/srv/uploads/j10/a", // not a path-boundary match
		"/srv/uploads/j1.bak":                    "/srv/uploads/j1.bak",
		"/opt/media-manipulator-ai/models/x":     "/opt/media-manipulator-ai/models/x",
		"srv/uploads/j1 plain text /srv/uploads": "srv/uploads/j1 plain text /srv/uploads",
		"prefix/srv/uploads/j1/a":                "prefix/srv/uploads/j1/a", // not at a token start
		"/usr/bin/ffmpeg":                        "/usr/bin/ffmpeg",
	}
	for in, want := range cases {
		if got := m.RewriteString(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	// A cmd round trip leaves the input untouched.
	c := Cmd{Exe: "x", Args: []string{"/srv/uploads/j1/a"}, Dir: "/srv/temp/j1"}
	r, err := m.RewriteCmd(c)
	if err != nil || c.Args[0] != "/srv/uploads/j1/a" || !strings.HasPrefix(r.Args[0], base) || !strings.HasPrefix(r.Dir, base) {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestMapper_DistinctBasenamesNeverCollide(t *testing.T) {
	m := mapperFor(t, Spec{Steps: []Cmd{{Exe: "x"}}, Inputs: []Path{P("/a/same.wav"), P("/b/same.wav")}})
	if m.Maps[0].Remote == m.Maps[1].Remote {
		t.Fatal("collision")
	}
}

func TestMapper_CardArgsEachPattern(t *testing.T) {
	m := mapperFor(t, Spec{Steps: []Cmd{{Exe: "x"}}})
	m.RemoteCard = 0
	cases := []struct {
		name string
		in   Cmd
		want Cmd
	}{
		{"env", Cmd{Exe: "x", Env: []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=3"}, Card: &CardArgs{Local: 3, Env: []string{CardEnvCUDA}}},
			Cmd{Exe: "x", Env: []string{"CUDA_DEVICE_ORDER=PCI_BUS_ID", "CUDA_VISIBLE_DEVICES=0"}}},
		{"-g", Cmd{Exe: "x", Args: []string{"-i", "3", "-g", "3"}, Card: &CardArgs{Local: 3, Flags: []string{CardFlagG}}},
			Cmd{Exe: "x", Args: []string{"-i", "3", "-g", "0"}}}, // only the declared flag's value
		{"--gpu", Cmd{Exe: "x", Args: []string{"--gpu", "2"}, Card: &CardArgs{Local: 2, Flags: []string{CardFlagGPU}}}, Cmd{Exe: "x", Args: []string{"--gpu", "0"}}},
		{"--gpu=", Cmd{Exe: "x", Args: []string{"--gpu=2"}, Card: &CardArgs{Local: 2, Flags: []string{CardFlagGPU}}}, Cmd{Exe: "x", Args: []string{"--gpu=0"}}},
		{"--device_index", Cmd{Exe: "x", Args: []string{"--device_index", "5"}, Card: &CardArgs{Local: 5, Flags: []string{CardFlagDeviceIdx}}}, Cmd{Exe: "x", Args: []string{"--device_index", "0"}}},
	}
	for _, tc := range cases {
		got, err := m.RewriteCmd(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		got.Card = nil
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v want %+v", tc.name, got, tc.want)
		}
	}
	for name, c := range map[string]Cmd{
		"missing env":   {Exe: "x", Card: &CardArgs{Local: 1, Env: []string{CardEnvCUDA}}},
		"wrong value":   {Exe: "x", Env: []string{"CUDA_VISIBLE_DEVICES=4"}, Card: &CardArgs{Local: 1, Env: []string{CardEnvCUDA}}},
		"missing flag":  {Exe: "x", Args: []string{"-i", "1"}, Card: &CardArgs{Local: 1, Flags: []string{CardFlagG}}},
		"wrong flagval": {Exe: "x", Args: []string{"-g", "9"}, Card: &CardArgs{Local: 1, Flags: []string{CardFlagG}}},
	} {
		if _, err := m.RewriteCmd(c); !errors.Is(err, ErrCardArgs) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Undeclared card numbers are never guessed at.
	got, _ := m.RewriteCmd(Cmd{Exe: "x", Args: []string{"-g", "3"}})
	if got.Args[1] != "3" {
		t.Error("undeclared card rewritten")
	}
}
