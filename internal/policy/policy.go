// Package policy implements lexical, root-contained scan selection rules.
//
// A policy revision is an ordered list of rules with three actions:
//
//   - include:    candidate paths matching the policy are in scope;
//   - exclude:    matching paths are left out of the snapshot;
//   - exception:  matching paths are kept even when an exclude also matches.
//
// Rules are evaluated strictly in order and the LAST matching rule decides
// (later rules override earlier ones; place exceptions after the excludes
// they override). When a revision declares at least one include rule, paths
// matching no rule at all are excluded by the include filter.
//
// All matching is purely lexical on slash-separated paths relative to the
// snapshot root. There is no filesystem access here: no "..", no absolute
// paths, and symlink targets are never interpreted, so a rule cannot reach
// outside the root. Scanning itself never follows symlinks.
package policy

import (
	"fmt"
	"path"
	"strings"
)

// Action values for user-supplied rules. ActionFilter is synthetic: it only
// appears in persisted selection evidence when a path is excluded solely
// because the revision has include rules and the path matched none of them.
type Action string

const (
	ActionInclude   Action = "include"
	ActionExclude   Action = "exclude"
	ActionException Action = "exception"
	ActionFilter    Action = "filter"
)

// MaxRules/MaxPatternLen bound how large one revision may be.
const (
	MaxRules      = 1000
	MaxPatternLen = 4096
)

// Rule is one ordered selection rule. Order is assigned by position in the
// revision (the API does not trust client-supplied order values); Pattern is
// a slash-relative glob.
type Rule struct {
	Order   int    `json:"order"`
	Action  Action `json:"action"`
	Pattern string `json:"pattern"`
}

// RuleError names one rejected rule.
type RuleError struct {
	Order  int    `json:"order"`
	Reason string `json:"reason"`
}

func (e *RuleError) Error() string {
	return fmt.Sprintf("rule %d: %s", e.Order, e.Reason)
}

// ValidationError aggregates every bad rule so callers see all problems at
// once instead of fixing them one request at a time.
type ValidationError struct {
	Errors []RuleError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, re := range e.Errors {
		parts[i] = re.Reason
	}
	return "invalid policy rules: " + strings.Join(parts, "; ")
}

// Validate checks rules without touching the filesystem:
//
//   - action must be include | exclude | exception;
//   - pattern must be slash-relative (no leading '/'), non-empty, with no
//     empty, "." or ".." segments — lexical containment only;
//   - "**" may only appear as a complete path segment;
//   - per-segment glob syntax must be accepted by path.Match;
//   - a pattern equal to "." conflicts with the snapshot root and is
//     rejected for every action.
func Validate(rules []Rule) error {
	var errs []RuleError
	if len(rules) > MaxRules {
		return &ValidationError{Errors: []RuleError{{
			Reason: fmt.Sprintf("too many rules: %d > %d", len(rules), MaxRules),
		}}}
	}
	for i, r := range rules {
		add := func(reason string) {
			errs = append(errs, RuleError{Order: i, Reason: reason})
		}
		switch r.Action {
		case ActionInclude, ActionExclude, ActionException:
		default:
			add(fmt.Sprintf("unknown action %q (want include|exclude|exception)", r.Action))
			continue
		}
		p := r.Pattern
		if len(p) > MaxPatternLen {
			add(fmt.Sprintf("pattern too long: %d > %d", len(p), MaxPatternLen))
			continue
		}
		if p == "" {
			add("empty pattern")
			continue
		}
		if strings.ContainsRune(p, 0) {
			add("pattern contains NUL byte")
			continue
		}
		if strings.HasPrefix(p, "/") {
			add("absolute patterns are not allowed (rules are relative to the snapshot root)")
			continue
		}
		if p == "." || p == "./" || p == ".." {
			add("pattern conflicts with the snapshot root or escapes it")
			continue
		}
		body := p
		if strings.HasSuffix(body, "/") {
			body = body[:len(body)-1]
		}
		if body == "" || body == "." || body == ".." {
			add("pattern conflicts with the snapshot root or escapes it")
			continue
		}
		segs := strings.Split(body, "/")
		bad := false
		for _, seg := range segs {
			switch seg {
			case "":
				add("empty path segment in pattern (repeated '/')")
				bad = true
			case ".":
				add("reserved '.' segment in pattern (rules are lexical and relative)")
				bad = true
			case "..":
				add("'..' segments are not allowed (rules cannot escape the root)")
				bad = true
			}
			if bad {
				break
			}
			if strings.Contains(seg, "**") && seg != "**" {
				add("'**' is only allowed as a complete path segment")
				bad = true
				break
			}
			if seg != "**" {
				// path.Match validates '?','*','[...]' syntax; '*' inside one
				// segment cannot cross '/' because matching is per segment.
				if _, err := path.Match(seg, "probe"); err != nil {
					add("bad glob syntax: " + err.Error())
					bad = true
					break
				}
			}
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// RuleMatch records that one rule matched a path. Decisive marks the last
// matching rule, whose action produced the final decision.
type RuleMatch struct {
	Rule     Rule `json:"rule"`
	Decisive bool `json:"decisive"`
}

// Decision is the evaluation result for one walked path.
type Decision struct {
	Included bool        `json:"included"`
	Matches  []RuleMatch `json:"matches,omitempty"`
	// FilterDefault is true when the path was excluded solely because the
	// revision contains include rules and no rule matched it.
	FilterDefault bool `json:"filter_default,omitempty"`
	// ExcludedBy is set by Walker when an ancestor directory's exclusion
	// closed this path's subtree. It holds that ancestor's slash path.
	ExcludedBy string `json:"excluded_by,omitempty"`
}

type compiledRule struct {
	Rule
	matchText string   // pattern with a trailing '/' stripped, used for matching
	dirOnly   bool     // pattern ended with '/'
	basename  bool     // pattern contains no '/' -> match last segment at any depth
	segments  []string // anchored segments when !basename
}

// Compiled is an immutable, ready-to-evaluate rule set. Compiling a revision
// is what the snapshot freezes at scan start.
type Compiled struct {
	rules      []compiledRule
	hasInclude bool
}

// Compile validates and compiles rules, assigning order by position. The
// authored Pattern text (including a directory-only trailing '/') is
// preserved, so the frozen document recompiles to identical semantics.
func Compile(rules []Rule) (*Compiled, error) {
	if err := Validate(rules); err != nil {
		return nil, err
	}
	c := &Compiled{}
	for i, r := range rules {
		r.Order = i
		cr := compiledRule{Rule: r}
		p := r.Pattern
		if strings.HasSuffix(p, "/") {
			cr.dirOnly = true
			p = p[:len(p)-1]
		}
		cr.matchText = p
		if strings.Contains(p, "/") {
			cr.segments = strings.Split(p, "/")
		} else {
			cr.basename = true
		}
		c.rules = append(c.rules, cr)
		if r.Action == ActionInclude {
			c.hasInclude = true
		}
	}
	return c, nil
}

// Rules returns the ordered rules as compiled (used to persist the freeze).
func (c *Compiled) Rules() []Rule {
	out := make([]Rule, len(c.rules))
	for i, cr := range c.rules {
		out[i] = cr.Rule
	}
	return out
}

// Decide evaluates one slash-relative path. The root itself ("." or "") is
// always in scope: no user rule can target it (Validate forbids ".").
func (c *Compiled) Decide(rel string, isDir bool) Decision {
	if rel == "" || rel == "." {
		return Decision{Included: true}
	}
	included := !c.hasInclude
	d := Decision{Included: included}
	last := -1
	for i := range c.rules {
		if !c.rules[i].matches(rel, isDir) {
			continue
		}
		switch c.rules[i].Action {
		case ActionExclude:
			d.Included = false
		default: // include and exception both force inclusion
			d.Included = true
		}
		d.Matches = append(d.Matches, RuleMatch{Rule: c.rules[i].Rule})
		last = i
	}
	for i := range d.Matches {
		d.Matches[i].Decisive = d.Matches[i].Rule.Order == last
	}
	if !d.Included && len(d.Matches) == 0 {
		d.FilterDefault = true
	}
	return d
}

func (cr compiledRule) matches(rel string, isDir bool) bool {
	if cr.dirOnly && !isDir {
		return false
	}
	segs := strings.Split(rel, "/")
	if cr.basename {
		ok, _ := path.Match(cr.matchText, segs[len(segs)-1])
		return ok
	}
	return matchSegments(cr.segments, segs)
}

// matchSegment treats pattern segments with path.Match ('*' does not cross
// '/'), while a standalone "**" matches zero or more complete path segments
// (gitignore semantics: "build/**" also matches "build" itself, and
// "**/*.bak" matches a top-level "x.bak" too).
func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			// Either consume nothing (skip the **) or consume one segment and
			// try again — this enumerates zero-or-more segment matches.
			if matchSegments(pattern[1:], name) {
				return true
			}
			if len(name) == 0 {
				return false
			}
			return matchSegments(pattern, name[1:])
		}
		if len(name) == 0 {
			return false
		}
		ok, _ := path.Match(pattern[0], name[0])
		if !ok {
			return false
		}
		pattern = pattern[1:]
		name = name[1:]
	}
	return len(name) == 0
}
