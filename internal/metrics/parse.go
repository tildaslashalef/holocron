package metrics

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Parsed is an exposition read back: each sample's value by its name and
// labels as written (`name{a="b"}`), and each family's type.
type Parsed struct {
	Samples map[string]float64
	Types   map[string]string
}

var (
	nameRe  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Parse reads the text format strictly, for tests and checks: every
// family has one HELP and one TYPE before its samples, every sample
// belongs to the family last declared (histograms' _bucket, _sum and
// _count included), label names are valid and label values escaped.
func Parse(text string) (*Parsed, error) {
	p := &Parsed{Samples: map[string]float64{}, Types: map[string]string{}}
	helps := map[string]bool{}
	var family string
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		fail := func(f string, a ...any) error { return fmt.Errorf("line %d %q: %s", n, line, fmt.Sprintf(f, a...)) }
		switch {
		case line == "":
			return nil, fail("empty line")
		case strings.HasPrefix(line, "# HELP "):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !nameRe.MatchString(name) || helps[name] {
				return nil, fail("bad or repeated HELP")
			}
			helps[name], family = true, name
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(strings.TrimPrefix(line, "# TYPE "))
			if len(f) != 2 || f[0] != family || p.Types[f[0]] != "" {
				return nil, fail("TYPE not right after its HELP, or repeated")
			}
			switch f[1] {
			case "counter", "gauge", "histogram":
			default:
				return nil, fail("unknown type")
			}
			p.Types[f[0]] = f[1]
		case strings.HasPrefix(line, "#"):
			return nil, fail("unknown comment")
		default:
			key, value, err := parseSample(line)
			if err != nil {
				return nil, fail("%v", err)
			}
			name, _, _ := strings.Cut(key, "{")
			if !belongs(name, family, p.Types[family]) {
				return nil, fail("sample outside its family %s", family)
			}
			if _, dup := p.Samples[key]; dup {
				return nil, fail("repeated sample")
			}
			p.Samples[key] = value
		}
	}
	return p, sc.Err()
}

func belongs(name, family, typ string) bool {
	if typ == "" {
		return false
	}
	if name == family {
		return typ != "histogram"
	}
	return typ == "histogram" && (name == family+"_bucket" || name == family+"_sum" || name == family+"_count")
}

// parseSample splits `name{a="b",…} value` and checks its labels.
func parseSample(line string) (key string, value float64, err error) {
	sp := strings.LastIndexByte(line, ' ')
	if sp < 0 {
		return "", 0, fmt.Errorf("no value")
	}
	key, raw := line[:sp], line[sp+1:]
	switch raw {
	case "+Inf", "-Inf", "NaN":
		value, _ = strconv.ParseFloat(raw, 64)
	default:
		if value, err = strconv.ParseFloat(raw, 64); err != nil {
			return "", 0, fmt.Errorf("value %q", raw)
		}
	}
	name, rest, hasLabels := strings.Cut(key, "{")
	if !nameRe.MatchString(name) {
		return "", 0, fmt.Errorf("name %q", name)
	}
	if !hasLabels {
		return key, value, nil
	}
	if !strings.HasSuffix(rest, "}") {
		return "", 0, fmt.Errorf("unclosed labels")
	}
	rest = strings.TrimSuffix(rest, "}")
	seen := map[string]bool{}
	for rest != "" {
		lname, after, ok := strings.Cut(rest, `="`)
		if !ok || !labelRe.MatchString(lname) || seen[lname] {
			return "", 0, fmt.Errorf("label name %q", lname)
		}
		seen[lname] = true
		i := 0
		for ; i < len(after); i++ {
			if after[i] == '\\' {
				if i+1 >= len(after) || !strings.ContainsRune(`\"n`, rune(after[i+1])) {
					return "", 0, fmt.Errorf("bad escape in %s", lname)
				}
				i++
				continue
			}
			if after[i] == '"' {
				break
			}
			if after[i] == '\n' {
				return "", 0, fmt.Errorf("raw newline in %s", lname)
			}
		}
		if i >= len(after) {
			return "", 0, fmt.Errorf("unterminated value of %s", lname)
		}
		rest = after[i+1:]
		if rest != "" {
			if rest[0] != ',' {
				return "", 0, fmt.Errorf("no comma after %s", lname)
			}
			rest = rest[1:]
		}
	}
	return key, value, nil
}
