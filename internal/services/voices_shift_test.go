package services

import (
	"math"
	"reflect"
	"testing"
)

// MM77 acceptance 7: golden ffmpeg argument lists per preset and for explicit
// semitones (+3 → pitch=1.189207, formant preserved vs shifted), clamp at ±12.
func TestVoiceShiftArgsGolden(t *testing.T) {
	const in, out = "/in/input.m4a", "/out/speech.wav"
	golden := func(af string) []string {
		return []string{"-hide_banner", "-nostdin", "-y", "-i", in, "-vn", "-sn", "-dn", "-af", af, "-c:a", "pcm_s16le", out}
	}
	cases := []struct {
		name      string
		preset    string
		semitones float64
		formant   string
		want      []string
	}{
		{"+3 preserved", "", 3, "preserved", golden("rubberband=pitch=1.189207:formant=preserved")},
		{"+3 shifted", "", 3, "shifted", golden("rubberband=pitch=1.189207:formant=shifted")},
		{"+3 default formant", "", 3, "", golden("rubberband=pitch=1.189207:formant=preserved")},
		{"-5 preserved", "", -5, "preserved", golden("rubberband=pitch=0.749154:formant=preserved")},
		{"0", "", 0, "", golden("rubberband=pitch=1.000000:formant=preserved")},
		{"clamp +20", "", 20, "", golden("rubberband=pitch=2.000000:formant=preserved")},
		{"clamp -30", "", -30, "shifted", golden("rubberband=pitch=0.500000:formant=shifted")},
		{"deeper", "deeper", 0, "", golden("rubberband=pitch=0.793701:formant=shifted")},
		{"higher", "higher", 0, "", golden("rubberband=pitch=1.259921:formant=shifted")},
		{"child", "child", 0, "", golden("rubberband=pitch=1.498307:formant=shifted")},
		{"elder", "elder", 0, "", golden("rubberband=pitch=0.890899:formant=preserved")},
		{"telephone", "telephone", 0, "", golden("rubberband=pitch=1.000000:formant=preserved,highpass=f=300,lowpass=f=3400")},
		{"preset wins over semitones", "DEEPER", 9, "preserved", golden("rubberband=pitch=0.793701:formant=shifted")},
	}
	for _, tc := range cases {
		s, err := ResolveVoiceShift(tc.preset, tc.semitones, tc.formant)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := VoiceShiftArgs(in, out, s); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

func TestVoiceShiftRejects(t *testing.T) {
	if _, err := ResolveVoiceShift("robot", 0, ""); err == nil {
		t.Error("unknown preset")
	}
	if _, err := ResolveVoiceShift("", 2, "sideways"); err == nil {
		t.Error("unknown formant")
	}
	if _, err := ResolveVoiceShift("", math.NaN(), ""); err == nil {
		t.Error("NaN semitones")
	}
}

func TestVoiceShiftPresetTableIsStable(t *testing.T) {
	ids := []string{}
	for _, p := range VoiceShiftPresets {
		ids = append(ids, p.ID)
		if math.Abs(p.Semitones) > VoiceShiftMaxSemitones || (p.Formant != FormantPreserved && p.Formant != FormantShifted) {
			t.Errorf("preset %s out of range: %+v", p.ID, p)
		}
	}
	if !reflect.DeepEqual(ids, []string{"deeper", "higher", "child", "elder", "telephone"}) {
		t.Fatalf("the UI mirrors this table; ids changed: %v", ids)
	}
}

func TestVoiceEncodeArgs(t *testing.T) {
	if got := VoiceMP3Args("a.wav", "a.mp3"); !reflect.DeepEqual(got, []string{"-hide_banner", "-nostdin", "-y", "-i", "a.wav", "-vn", "-c:a", "libmp3lame", "-q:a", "2", "a.mp3"}) {
		t.Errorf("mp3 args %q", got)
	}
	if got := VoiceWavArgs("in", "out.wav", 30); !reflect.DeepEqual(got, []string{"-hide_banner", "-nostdin", "-y", "-i", "in", "-vn", "-sn", "-dn", "-t", "30", "-ac", "1", "-ar", "24000", "-c:a", "pcm_s16le", "out.wav"}) {
		t.Errorf("wav args %q", got)
	}
}
