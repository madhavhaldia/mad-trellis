package buildinfo

import "testing"

func TestImageCached(t *testing.T) {
	ls := "NAME        TAG     DIGEST\n" +
		"alpine/git  latest  8d6ede0b29c6\n" +
		"alpine      latest  28bd5fe8b56d\n"
	cases := []struct {
		image string
		want  bool
	}{
		{"alpine:latest", true},
		{"alpine", true}, // bare name → any tag
		{"alpine/git:latest", true},
		{"alpine/git", true},
		{"alpine:3.19", false}, // wrong tag
		{"busybox", false},     // absent
		{"busybox:latest", false},
		{"registry:5000/img", false}, // colon-before-slash is a port, not a tag; absent
	}
	for _, c := range cases {
		if got := ImageCached(ls, c.image); got != c.want {
			t.Errorf("ImageCached(%q) = %v, want %v", c.image, got, c.want)
		}
	}
}

func TestImageCachedEmptyOutput(t *testing.T) {
	if ImageCached("", "alpine:latest") {
		t.Fatal("empty output must not match")
	}
	if ImageCached("NAME TAG DIGEST\n", "alpine") {
		t.Fatal("header-only output must not match")
	}
}
