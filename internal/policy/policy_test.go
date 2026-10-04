package policy

import (
	"strings"
	"testing"
)

func TestValidateRejectsEscapeAndRootConflicts(t *testing.T) {
	bad := []Rule{
		{Action: ActionExclude, Pattern: "../etc/passwd"},
		{Action: ActionExclude, Pattern: "a/../../b"},
		{Action: ActionExclude, Pattern: "/abs/path"},
		{Action: ActionExclude, Pattern: "."},
		{Action: ActionExclude, Pattern: "./"},
		{Action: ActionExclude, Pattern: "a/../b"},
		{Action: ActionExclude, Pattern: "a/**.tmp"},
		{Action: ActionExclude, Pattern: "a/[b"},
		{Action: ActionExclude, Pattern: ""},
		{Action: ActionExclude, Pattern: "a//b"},
		{Action: ActionExclude, Pattern: "a/./b"},
		{Action: "weird", Pattern: "ok"},
		{Action: ActionExclude, Pattern: "a/**/b"}, // this one is legal
	}
	for _, r := range bad {
		r := r
		if r.Pattern == "a/**/b" {
			if err := Validate([]Rule{r}); err != nil {
				t.Errorf("%q unexpectedly rejected: %v", r.Pattern, err)
			}
			continue
		}
		if err := Validate([]Rule{r}); err == nil {
			t.Errorf("%q should be rejected", r.Pattern)
		}
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	err := Validate([]Rule{
		{Action: ActionExclude, Pattern: "../a"},
		{Action: ActionExclude, Pattern: "/b"},
	})
	ve, ok := err.(*ValidationError)
	if !ok || len(ve.Errors) != 2 {
		t.Fatalf("want 2 RuleErrors, got %v", err)
	}
}

func TestLastMatchWinsAndExceptionOverrides(t *testing.T) {
	c, err := Compile([]Rule{
		{Action: ActionExclude, Pattern: "*.tmp"},
		{Action: ActionException, Pattern: "keep.tmp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := c.Decide("scratch.tmp", false)
	if d.Included {
		t.Fatal("scratch.tmp must be excluded")
	}
	if len(d.Matches) != 1 || !d.Matches[0].Decisive || d.Matches[0].Rule.Action != ActionExclude {
		t.Fatalf("scratch.tmp evidence wrong: %+v", d.Matches)
	}
	d = c.Decide("keep.tmp", false)
	if !d.Included {
		t.Fatal("keep.tmp must be kept by exception")
	}
	if len(d.Matches) != 2 || !d.Matches[1].Decisive || d.Matches[1].Rule.Action != ActionException {
		t.Fatalf("keep.tmp evidence wrong: %+v", d.Matches)
	}
	if d.FilterDefault {
		t.Fatal("matched path must not be filter-default")
	}
}

func TestIncludeFilterDefault(t *testing.T) {
	c, err := Compile([]Rule{
		{Action: ActionInclude, Pattern: "src/**"},
		{Action: ActionInclude, Pattern: "README"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		include bool
		filter  bool
	}{
		"src/a.go":      {true, false},
		"src/deep/x.go": {true, false},
		"README":        {true, false},
		"docs/note.md":  {false, true},
		"other.tmp":     {false, true},
	}
	for rel, want := range cases {
		d := c.Decide(rel, strings.HasSuffix(rel, "/"))
		if d.Included != want.include || d.FilterDefault != want.filter {
			t.Errorf("%s: included=%v filter=%v, want %v/%v",
				rel, d.Included, d.FilterDefault, want.include, want.filter)
		}
	}
}

func TestBasenameMatchesAtAnyDepth(t *testing.T) {
	c, _ := Compile([]Rule{{Action: ActionExclude, Pattern: "*.tmp"}})
	for _, rel := range []string{"a.tmp", "x/y/a.tmp", "x/y/z/a.tmp"} {
		if d := c.Decide(rel, false); d.Included {
			t.Errorf("%s must be excluded", rel)
		}
	}
	if d := c.Decide("x/y/a.log", false); !d.Included {
		t.Errorf("a.log must remain included: %+v", d)
	}
}

func TestTrailingSlashMatchesDirectoriesOnly(t *testing.T) {
	c, _ := Compile([]Rule{{Action: ActionExclude, Pattern: "cache/"}})
	if d := c.Decide("cache", true); d.Included {
		t.Error("directory cache must be excluded")
	}
	if d := c.Decide("cache", false); !d.Included {
		t.Error("file named cache must not match a directory-only rule")
	}
}

func TestDoubleStarSemantics(t *testing.T) {
	c, _ := Compile([]Rule{{Action: ActionExclude, Pattern: "build/**"}})
	for _, rel := range []string{"build", "build/out.o", "build/sub/x.o", "build/x"} {
		if d := c.Decide(rel, false); d.Included {
			t.Errorf("%s must be excluded (** matches zero or more segments)", rel)
		}
	}
	if d := c.Decide("building/x", false); !d.Included {
		t.Error("build/** must not match the sibling 'building'")
	}

	c2, _ := Compile([]Rule{{Action: ActionExclude, Pattern: "**/*.bak"}})
	for _, rel := range []string{"x.bak", "a/x.bak", "a/b/x.bak"} {
		if d := c2.Decide(rel, false); d.Included {
			t.Errorf("%s must be excluded", rel)
		}
	}
	for _, rel := range []string{"keep", "d/keep", "d/e/keep"} {
		if d := c2.Decide(rel, false); !d.Included {
			t.Errorf("%s must remain included", rel)
		}
	}
}

func TestOrderIsPositionAssigned(t *testing.T) {
	c, err := Compile([]Rule{
		{Order: 99, Action: ActionExclude, Pattern: "*.tmp"},
		{Order: 5, Action: ActionException, Pattern: "x.tmp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rs := c.Rules()
	if rs[0].Order != 0 || rs[1].Order != 1 {
		t.Fatalf("client order must be ignored: %+v", rs)
	}
}

func TestRootAlwaysIncluded(t *testing.T) {
	c, _ := Compile([]Rule{{Action: ActionExclude, Pattern: "*"}})
	for _, root := range []string{".", ""} {
		if d := c.Decide(root, true); !d.Included {
			t.Errorf("root %q must always be included", root)
		}
	}
}
