package policy

import "testing"

func TestWalkerAncestorExclusion(t *testing.T) {
	c, err := Compile([]Rule{
		{Action: ActionExclude, Pattern: "cache/"},
		{Action: ActionException, Pattern: "cache/keep.tmp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := NewWalker(c)

	// parent dir excluded
	if d := w.Visit("cache", true); d.Included {
		t.Fatal("cache/ must be excluded")
	}
	// children inherit the exclusion regardless of their own rules — an
	// exception cannot resurrect a file through a closed directory
	for _, p := range []struct {
		rel   string
		isDir bool
	}{
		{"cache/a.tmp", false},
		{"cache/sub", true},
		{"cache/sub/deep.bin", false},
		{"cache/keep.tmp", false}, // explicit exception still cannot escape
	} {
		d := w.Visit(p.rel, p.isDir)
		if d.Included {
			t.Errorf("%s must inherit ancestor exclusion", p.rel)
		}
		if d.ExcludedBy != "cache" {
			t.Errorf("%s ExcludedBy=%q want cache", p.rel, d.ExcludedBy)
		}
		if len(d.Matches) != 1 || d.Matches[0].Rule.Pattern != "cache/" || !d.Matches[0].Decisive {
			t.Errorf("%s inherited rule evidence wrong: %+v", p.rel, d.Matches)
		}
	}

	// siblings unaffected
	if d := w.Visit("other.tmp", false); !d.Included {
		t.Error("sibling must remain included")
	}
}

func TestWalkerNestedDirsIndependence(t *testing.T) {
	c, _ := Compile([]Rule{{Action: ActionExclude, Pattern: "a/cache/"}})
	w := NewWalker(c)
	if d := w.Visit("a", true); !d.Included {
		t.Error("a/ included")
	}
	if d := w.Visit("a/cache", true); d.Included {
		t.Error("a/cache excluded")
	}
	if d := w.Visit("a/cache/f", false); d.Included || d.ExcludedBy != "a/cache" {
		t.Errorf("nested inherit: included=%v by=%q", d.Included, d.ExcludedBy)
	}
	if d := w.Visit("a/keep", false); !d.Included {
		t.Error("a/keep must remain included")
	}
}
