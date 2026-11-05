package evals

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// MM116a: `expect.options` checks on the options of an open_tool button, so the
// Options suite grades what the model put into the form, not just which tool.
//
//	compress-video.level = smallest
//	compress-video.crf in 28..40            (numeric range, inclusive)
//	compress-video.resolution in [480|720]  (one of)
//	compress-video.crf >= 28                (also <=, <, >, !=)
//	compress-video.stripAudio = true
//	compress-video.crf present              (the field is set, any value)
//	compress-video.options absent           (the button carries no options)
//	A || B                                  (either expression holds)
//
// Each expression reads the first open_tool action (or, MM116b, confirm_job card) for the slug. Pure.

var optionExprRe = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*)\.([A-Za-z][A-Za-z0-9]*)\s*(.*)$`)

// OptionCheck is one parsed `A || B` expression.
type OptionCheck struct {
	src  string
	alts []optionTerm
}

type optionTerm struct {
	slug, field, op string
	num             float64
	hasNum          bool
	lo, hi          float64
	set             []string
	str             string
}

// ParseOptionCheck parses one expect.options entry.
func ParseOptionCheck(src string) (*OptionCheck, error) {
	c := &OptionCheck{src: src}
	for _, part := range strings.Split(src, "||") {
		t, err := parseOptionTerm(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("options check %q: %w", src, err)
		}
		c.alts = append(c.alts, t)
	}
	return c, nil
}

func parseOptionTerm(s string) (optionTerm, error) {
	m := optionExprRe.FindStringSubmatch(s)
	if m == nil {
		return optionTerm{}, fmt.Errorf("want <slug>.<field> <op> <value>")
	}
	t := optionTerm{slug: m[1], field: m[2]}
	rest := strings.TrimSpace(m[3])
	switch {
	case rest == "present" || rest == "absent":
		t.op = rest
	case strings.HasPrefix(rest, "in "):
		arg := strings.TrimSpace(rest[3:])
		if strings.HasPrefix(arg, "[") && strings.HasSuffix(arg, "]") {
			t.op = "inset"
			for _, v := range strings.Split(arg[1:len(arg)-1], "|") {
				t.set = append(t.set, strings.TrimSpace(v))
			}
		} else if lo, hi, ok := strings.Cut(arg, ".."); ok {
			var err1, err2 error
			t.lo, err1 = strconv.ParseFloat(strings.TrimSpace(lo), 64)
			t.hi, err2 = strconv.ParseFloat(strings.TrimSpace(hi), 64)
			if err1 != nil || err2 != nil || t.lo > t.hi {
				return t, fmt.Errorf("bad range %q", arg)
			}
			t.op = "range"
		} else {
			return t, fmt.Errorf("in wants a..b or [x|y]")
		}
	default:
		for _, op := range []string{"<=", ">=", "!=", "=", "<", ">"} {
			if strings.HasPrefix(rest, op) {
				t.op = op
				t.str = strings.TrimSpace(rest[len(op):])
				if f, err := strconv.ParseFloat(t.str, 64); err == nil {
					t.num, t.hasNum = f, true
				}
				break
			}
		}
		if t.op == "" || t.str == "" {
			return t, fmt.Errorf("unknown operator in %q", rest)
		}
		if (t.op != "=" && t.op != "!=") && !t.hasNum {
			return t, fmt.Errorf("%s needs a number", t.op)
		}
	}
	return t, nil
}

// Slugs lists the tool slugs the expression reads (the case-file test checks them).
func (c *OptionCheck) Slugs() []string {
	var out []string
	for _, a := range c.alts {
		out = append(out, a.slug)
	}
	return out
}

// Fields lists slug.field pairs ("options" is the pseudo-field of `absent`).
func (c *OptionCheck) Fields() [][2]string {
	var out [][2]string
	for _, a := range c.alts {
		out = append(out, [2]string{a.slug, a.field})
	}
	return out
}

// Eval checks the expression against the actions of one answer.
func (c *OptionCheck) Eval(acts []Action) (bool, string) {
	var why []string
	for _, a := range c.alts {
		ok, w := a.eval(acts)
		if ok {
			return true, ""
		}
		why = append(why, w)
	}
	return false, strings.Join(why, " and ")
}

func (t optionTerm) eval(acts []Action) (bool, string) {
	var act *Action
	for i := range acts {
		if acts[i].Kind == "open_tool" && strParam(acts[i].Params, "slug") == t.slug {
			act = &acts[i]
			break
		}
		// MM116b: a confirm_job card carries the same validated options for its tool.
		if acts[i].Kind == "confirm_job" && strParam(acts[i].Params, "tool") == t.slug {
			act = &acts[i]
			break
		}
	}
	if act == nil {
		return false, fmt.Sprintf("no open_tool button or confirm_job card for %s", t.slug)
	}
	opts, _ := act.Params["options"].(map[string]any)
	if t.field == "options" {
		if t.op == "absent" {
			return len(opts) == 0, fmt.Sprintf("%s button carries options %v", t.slug, opts)
		}
		return len(opts) > 0, fmt.Sprintf("%s button has no options", t.slug)
	}
	v, has := opts[t.field]
	switch t.op {
	case "present":
		return has, fmt.Sprintf("%s.%s not set (options %v)", t.slug, t.field, opts)
	case "absent":
		return !has, fmt.Sprintf("%s.%s is set to %v", t.slug, t.field, v)
	}
	if !has {
		return false, fmt.Sprintf("%s.%s not set (options %v)", t.slug, t.field, opts)
	}
	got := fmt.Sprint(v)
	f, isNum := v.(float64)
	switch t.op {
	case "inset":
		for _, s := range t.set {
			if s == got {
				return true, ""
			}
		}
		return false, fmt.Sprintf("%s.%s = %s, want one of %v", t.slug, t.field, got, t.set)
	case "range":
		return isNum && f >= t.lo && f <= t.hi, fmt.Sprintf("%s.%s = %s, want %v..%v", t.slug, t.field, got, t.lo, t.hi)
	case "=", "!=":
		eq := got == t.str
		if isNum && t.hasNum {
			eq = math.Abs(f-t.num) < 1e-9
		}
		return eq == (t.op == "="), fmt.Sprintf("%s.%s = %s, want %s %s", t.slug, t.field, got, t.op, t.str)
	default:
		if !isNum {
			return false, fmt.Sprintf("%s.%s = %s is not a number", t.slug, t.field, got)
		}
		var ok bool
		switch t.op {
		case "<=":
			ok = f <= t.num
		case ">=":
			ok = f >= t.num
		case "<":
			ok = f < t.num
		case ">":
			ok = f > t.num
		}
		return ok, fmt.Sprintf("%s.%s = %s, want %s %s", t.slug, t.field, got, t.op, t.str)
	}
}
