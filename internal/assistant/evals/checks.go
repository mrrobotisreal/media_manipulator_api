package evals

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Op-check DSL (ticket decision 5). One expression per string:
//
//	has_op(grey_except)                     an op named grey_except exists
//	no_op(delete_clip)                      no such op
//	op_count <= 4                           total ops (also <, >, >=, ==)
//	has_layer(hsl)                          an add_layer of that type (photo) or that layer type (vector add_layer)
//	param(hsl, ranges.0.saturation) < 0     a numeric field of the first add_layer of that type
//	                                        (dot path; `≈` / `~=` = within 10 %); use `*` as the type to look at any op
//	field(grade, lumetri.saturation) < 1    a numeric field of the first op with that name
//	keyframe_within(transform.x, 600)       an animate/set_keyframes op on that property whose
//	                                        keyframes all lie within N ms (vector) or N/1000 s (studio)
//	title_in_region(lower30)                an add_title lowerThird, or text/rect layers with y in the lower 30 %
//	mentions_range(red)                     a grey_except range named red (or a hue range covering it)
//	a || b                                  either alternative passes (models may pick another valid route)
//
// Every primitive has a unit test on canned ops (checks_test.go).

// Check is one parsed expression.
type Check struct {
	src  string
	eval func(ops []map[string]any) (bool, string)
}

// Eval runs the check on a turn's ops (all edits of the turn, in order).
func (c Check) Eval(ops []map[string]any) (bool, string) { return c.eval(ops) }

// String is the source expression.
func (c Check) String() string { return c.src }

var (
	callRe = regexp.MustCompile(`^([a-z_0-9]+)\(([^)]*)\)\s*(?:(<=|>=|==|<|>|≈|~=)\s*(-?[0-9.]+))?$`)
	cmpRe  = regexp.MustCompile(`^op_count\s*(<=|>=|==|<|>)\s*([0-9]+)$`)
)

// ParseCheck parses one DSL expression; `a || b` passes when either does.
func ParseCheck(src string) (Check, error) {
	if alts := strings.Split(src, "||"); len(alts) > 1 {
		var cs []Check
		for _, a := range alts {
			c, err := parseOne(a)
			if err != nil {
				return Check{}, err
			}
			cs = append(cs, c)
		}
		return Check{src, func(ops []map[string]any) (bool, string) {
			var whys []string
			for _, c := range cs {
				ok, why := c.Eval(ops)
				if ok {
					return true, ""
				}
				whys = append(whys, why)
			}
			return false, strings.Join(whys, " / ")
		}}, nil
	}
	return parseOne(src)
}

func parseOne(src string) (Check, error) {
	s := strings.TrimSpace(src)
	if m := cmpRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[2])
		return Check{src, func(ops []map[string]any) (bool, string) {
			return compare(float64(len(ops)), m[1], float64(n)), fmt.Sprintf("op_count = %d", len(ops))
		}}, nil
	}
	m := callRe.FindStringSubmatch(s)
	if m == nil {
		return Check{}, fmt.Errorf("op check %q: not a known expression", src)
	}
	name, args, op, val := m[1], splitArgs(m[2]), m[3], m[4]
	num := 0.0
	if op != "" {
		var err error
		if num, err = strconv.ParseFloat(val, 64); err != nil {
			return Check{}, fmt.Errorf("op check %q: bad number", src)
		}
	}
	need := func(n int, cmp bool) error {
		if len(args) != n {
			return fmt.Errorf("op check %q: %s takes %d argument(s)", src, name, n)
		}
		if cmp && op == "" {
			return fmt.Errorf("op check %q: %s needs a comparison", src, name)
		}
		if !cmp && op != "" {
			return fmt.Errorf("op check %q: %s takes no comparison", src, name)
		}
		return nil
	}
	switch name {
	case "has_op", "no_op":
		if err := need(1, false); err != nil {
			return Check{}, err
		}
		want := name == "has_op"
		return Check{src, func(ops []map[string]any) (bool, string) {
			found := len(opsNamed(ops, args[0])) > 0
			return found == want, fmt.Sprintf("ops: %s", opNames(ops))
		}}, nil
	case "has_layer":
		if err := need(1, false); err != nil {
			return Check{}, err
		}
		return Check{src, func(ops []map[string]any) (bool, string) {
			return layerOp(ops, args[0]) != nil, fmt.Sprintf("layers added: %s", layerTypes(ops))
		}}, nil
	case "param", "field":
		if err := need(2, true); err != nil {
			return Check{}, err
		}
		return Check{src, func(ops []map[string]any) (bool, string) {
			var target map[string]any
			if name == "param" {
				if args[0] == "*" {
					for _, o := range ops {
						if _, ok := lookupNum(o, "params."+args[1]); ok {
							target = o
							break
						}
					}
				} else {
					target = layerOp(ops, args[0])
				}
			} else if os := opsNamed(ops, args[0]); len(os) > 0 {
				target = os[0]
			}
			if target == nil {
				return false, fmt.Sprintf("no %s op/layer %s", name, args[0])
			}
			p := args[1]
			if name == "param" {
				p = "params." + p
			}
			v, ok := lookupNum(target, p)
			if !ok {
				return false, fmt.Sprintf("%s missing", args[1])
			}
			return compare(v, op, num), fmt.Sprintf("%s = %g", args[1], v)
		}}, nil
	case "keyframe_within":
		if err := need(2, false); err != nil {
			return Check{}, err
		}
		limit, err := strconv.ParseFloat(args[1], 64)
		if err != nil {
			return Check{}, fmt.Errorf("op check %q: limit must be a number of ms", src)
		}
		return Check{src, func(ops []map[string]any) (bool, string) {
			for _, o := range ops {
				n, _ := o["op"].(string)
				if (n != "animate" && n != "set_keyframes") || o["property"] != args[0] {
					continue
				}
				kfs, _ := o["keyframes"].([]any)
				if len(kfs) == 0 {
					continue
				}
				scale := 1.0
				if n == "set_keyframes" {
					scale = 1000 // studio keyframes are seconds
				}
				first, _ := asNum(kfs[0].(map[string]any)["t"])
				last, _ := asNum(kfs[len(kfs)-1].(map[string]any)["t"])
				// The entrance must finish within `limit` of its own start.
				if (last-first)*scale <= limit+1e-9 {
					return true, ""
				}
				return false, fmt.Sprintf("%s keyframes span %g ms", args[0], (last-first)*scale)
			}
			return false, "no keyframes on " + args[0]
		}}, nil
	case "title_in_region":
		if err := need(1, false); err != nil {
			return Check{}, err
		}
		if args[0] != "lower30" && args[0] != "upper30" && args[0] != "center" {
			return Check{}, fmt.Errorf("op check %q: region is lower30, upper30 or center", src)
		}
		return Check{src, func(ops []map[string]any) (bool, string) { return titleInRegion(ops, args[0]) }}, nil
	case "mentions_range":
		if err := need(1, false); err != nil {
			return Check{}, err
		}
		hue, ok := namedHues[args[0]]
		if !ok {
			return Check{}, fmt.Errorf("op check %q: unknown colour %s", src, args[0])
		}
		return Check{src, func(ops []map[string]any) (bool, string) {
			for _, o := range opsNamed(ops, "grey_except") {
				rs, _ := o["ranges"].([]any)
				for _, r := range rs {
					if s, ok := r.(string); ok && s == args[0] {
						return true, ""
					}
					if m, ok := r.(map[string]any); ok {
						h, _ := asNum(m["hue"])
						w, ok := asNum(m["width"])
						if !ok {
							w = 30
						}
						if hueDist(h, hue) <= w/2+15 {
							return true, ""
						}
					}
				}
			}
			return false, "no grey_except range for " + args[0]
		}}, nil
	}
	return Check{}, fmt.Errorf("op check %q: unknown primitive %s", src, name)
}

var namedHues = map[string]float64{"red": 0, "orange": 30, "yellow": 60, "green": 120, "aqua": 180, "blue": 240, "purple": 270, "magenta": 300}

func hueDist(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	if d > 180 {
		d = 360 - d
	}
	return d
}

func splitArgs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func compare(v float64, op string, n float64) bool {
	switch op {
	case "<":
		return v < n
	case "<=":
		return v <= n
	case ">":
		return v > n
	case ">=":
		return v >= n
	case "==":
		return v == n
	case "≈", "~=":
		return math.Abs(v-n) <= math.Max(math.Abs(n)*0.1, 1e-9)
	}
	return false
}

func opsNamed(ops []map[string]any, name string) []map[string]any {
	var out []map[string]any
	for _, o := range ops {
		if o["op"] == name {
			out = append(out, o)
		}
	}
	return out
}

func opNames(ops []map[string]any) string {
	var n []string
	for _, o := range ops {
		s, _ := o["op"].(string)
		n = append(n, s)
	}
	if len(n) == 0 {
		return "none"
	}
	return strings.Join(n, ", ")
}

// layerOp is the first add_layer (or apply_preset expanded by name never) of a type.
func layerOp(ops []map[string]any, typ string) map[string]any {
	for _, o := range ops {
		if o["op"] == "add_layer" && o["type"] == typ {
			return o
		}
	}
	return nil
}

func layerTypes(ops []map[string]any) string {
	var t []string
	for _, o := range ops {
		if o["op"] == "add_layer" {
			s, _ := o["type"].(string)
			t = append(t, s)
		}
	}
	if len(t) == 0 {
		return "none"
	}
	return strings.Join(t, ", ")
}

// lookupNum follows a dot path through maps and list indexes.
func lookupNum(m map[string]any, p string) (float64, bool) {
	var cur any = m
	for _, k := range strings.Split(p, ".") {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[k]
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(c) {
				return 0, false
			}
			cur = c[i]
		default:
			return 0, false
		}
	}
	return asNum(cur)
}

func asNum(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// titleInRegion: Darkroom add_title templates, or Motion text/rect layers whose
// y lies in the region of the canvas (height from a set_canvas op, else 512).
func titleInRegion(ops []map[string]any, region string) (bool, string) {
	for _, o := range ops {
		if o["op"] == "add_title" {
			t, _ := o["template"].(string)
			switch {
			case region == "lower30" && (t == "lowerThird" || t == "caption"):
				return true, ""
			case region == "center" && (t == "title" || t == "endCard"):
				return true, ""
			}
		}
		if o["op"] == "template" {
			if k, _ := o["kind"].(string); region == "lower30" && k == "lowerThird" {
				return true, ""
			}
		}
	}
	h := 512.0
	for _, o := range opsNamed(ops, "set_canvas") {
		if v, ok := asNum(o["height"]); ok {
			h = v
		}
	}
	var seen []string
	for _, o := range ops {
		var y float64
		var ok bool
		switch o["op"] {
		case "add_text":
			y, ok = asNum(o["y"])
		case "add_layer":
			if props, _ := o["props"].(map[string]any); props != nil && (o["type"] == "text" || o["type"] == "rect") {
				y, ok = asNum(props["y"])
			}
		}
		if !ok {
			continue
		}
		f := y / h
		seen = append(seen, fmt.Sprintf("%.2f", f))
		switch region {
		case "lower30":
			if f >= 0.7 && f <= 1 {
				return true, ""
			}
		case "upper30":
			if f >= 0 && f <= 0.3 {
				return true, ""
			}
		case "center":
			if f > 0.3 && f < 0.7 {
				return true, ""
			}
		}
	}
	return false, fmt.Sprintf("no title in %s (text y fractions: %s)", region, strings.Join(seen, ", "))
}

// DetectLanguage is a coarse script-based guess, enough to tell "answered in
// the user's language" from "answered in English": ru/uk (Cyrillic; і/ї/є →
// uk), he, ar, hi, ja (kana), zh (Han without kana), ko, else Latin → en unless
// common function words of de/es/fr/it/pt/vi dominate.
func DetectLanguage(s string) string {
	counts := map[string]int{}
	uk := 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			counts["cyr"]++
			if strings.ContainsRune("іїєґІЇЄҐ", r) {
				uk++
			}
		case unicode.Is(unicode.Hebrew, r):
			counts["he"]++
		case unicode.Is(unicode.Arabic, r):
			counts["ar"]++
		case unicode.Is(unicode.Devanagari, r):
			counts["hi"]++
		case unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			counts["kana"]++
		case unicode.Is(unicode.Han, r):
			counts["han"]++
		case unicode.Is(unicode.Hangul, r):
			counts["ko"]++
		case unicode.IsLetter(r):
			counts["latin"]++
		}
	}
	best, n := "latin", 0
	for k, v := range counts {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	switch best {
	case "cyr":
		if uk > 0 {
			return "uk"
		}
		return "ru"
	case "kana":
		return "ja"
	case "han":
		if counts["kana"] > 0 {
			return "ja"
		}
		return "zh"
	case "latin":
		return latinLanguage(s)
	}
	return best
}

var latinMarkers = map[string][]string{
	"de": {" und ", " der ", " die ", " das ", " nicht ", " ist ", " mit ", " sie "},
	"es": {" el ", " los ", " que ", " para ", " una ", " con ", " puedes ", " del "},
	"fr": {" le ", " les ", " des ", " est ", " pour ", " une ", " vous ", " avec "},
	"it": {" il ", " che ", " per ", " una ", " con ", " puoi ", " della ", " gli "},
	"pt": {" o ", " os ", " que ", " para ", " uma ", " com ", " você ", " não "},
	"vi": {" và ", " của ", " là ", " cho ", " bạn ", " không ", " một ", " được "},
	"en": {" the ", " and ", " you ", " to ", " is ", " for ", " with ", " your "},
}

// latinLetters are letters that, among the site's Latin-script languages,
// only that language uses (studio-de-grade, 2026-11-05: a German undo label
// with no prose read as English).
var latinLetters = map[string]string{
	"de": "äöüß",
	"es": "ñ¿¡",
	"fr": "èêëœçû",
	"it": "ì",
	"pt": "ãõ",
	"vi": "ăđơưạảấầẩẫậắằẳẵặẹẻẽếềểễệỉịọỏốồổỗộớờởỡợụủứừửữựỳỵỷỹ",
}

func latinLanguage(s string) string {
	low := " " + strings.ToLower(strings.Join(strings.Fields(s), " ")) + " "
	best, n := "en", 0
	for _, lang := range []string{"en", "de", "es", "fr", "it", "pt", "vi"} {
		c := 0
		for _, w := range latinMarkers[lang] {
			c += strings.Count(low, w)
		}
		// Short texts (an undo label like "Farben kräftiger") rarely carry a
		// function word; letters only one language uses decide them.
		for _, r := range latinLetters[lang] {
			if strings.ContainsRune(low, r) {
				c++
			}
		}
		if c > n {
			best, n = lang, c
		}
	}
	return best
}
