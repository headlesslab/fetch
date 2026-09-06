package fetch

import "testing"

// The link resolver is what keeps an archive from reaching outside its
// destination through symbolic links, so its rules are pinned here with
// worked examples, independent of whether this machine can create links.
func TestResolveFollowsLinksTheWayTheOperatingSystemWould(t *testing.T) {
	x := &extractor{links: map[string]string{
		"a/b/up":     "..",    // a/b/up -> a
		"a/b/top":    "../..", // a/b/top -> the root
		"a/self":     "self",  // loops
		"a/one":      "two",   // a/one -> a/two -> a/three
		"a/two":      "three",
		"Current":    "152.0/dist", // Current -> 152.0/dist
		"lib/loop-a": "loop-b",
		"lib/loop-b": "loop-a",
	}}

	cases := map[string]struct {
		path string
		want string
		fail bool
	}{
		"plain":                      {path: "a/b/c.txt", want: "a/b/c.txt"},
		"dot and empty components":   {path: "./a//b/./c.txt", want: "a/b/c.txt"},
		"parent inside":              {path: "a/b/../c.txt", want: "a/c.txt"},
		"parent of root":             {path: "..", fail: true},
		"parent past root":           {path: "a/../../x", fail: true},
		"through a link":             {path: "a/b/up/c.txt", want: "a/c.txt"},
		"link then parent":           {path: "a/b/top/../x", fail: true},
		"chained links":              {path: "a/one/file", want: "a/three/file"},
		"link to a nested target":    {path: "Current/bin", want: "152.0/dist/bin"},
		"link to a directory dotdot": {path: "a/b/up/../x", want: "x"},
		"link to itself":             {path: "a/self", fail: true},
		"two links looping":          {path: "lib/loop-a/x", fail: true},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := x.resolve(c.path)

			if c.fail {
				if err == nil {
					t.Fatalf("resolve(%q) = %q, want an error", c.path, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolve(%q): %v", c.path, err)
			}

			if got != c.want {
				t.Fatalf("resolve(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestNotThroughLinkRefusesEveryPathUnderALink(t *testing.T) {
	x := &extractor{links: map[string]string{"a/link": "target"}}

	for _, name := range []string{"a/link", "a/link/x", "a/link/x/y.txt"} {
		if err := x.notThroughLink(name); err == nil {
			t.Errorf("notThroughLink(%q) = nil, want an error", name)
		}
	}

	for _, name := range []string{"a", "a/other", "a/linked/x", "b/link/x"} {
		if err := x.notThroughLink(name); err != nil {
			t.Errorf("notThroughLink(%q) = %v, want nil", name, err)
		}
	}
}
