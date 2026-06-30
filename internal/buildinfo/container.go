package buildinfo

import "strings"

// ImageCached reports whether `container image ls` output lists the given image
// ref. It is a PURE parse (no runtime) so doctor's container check is unit-tested
// without the Apple `container` runtime. The `container image ls` table is:
//
//	NAME        TAG     DIGEST
//	alpine/git  latest  8d6ede0b29c6
//	alpine      latest  28bd5fe8b56d
//
// image may be NAME ("alpine" → any tag matches) or NAME:TAG ("alpine:latest").
// A ":" is treated as a tag separator ONLY when it follows the last "/", so a
// registry-port ref ("registry:5000/img") is parsed as a bare name, not a tag.
func ImageCached(lsOutput, image string) bool {
	name, tag := image, ""
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		name, tag = image[:colon], image[colon+1:]
	}
	for _, line := range strings.Split(lsOutput, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "NAME" {
			continue // blank line or the header
		}
		if f[0] == name && (tag == "" || f[1] == tag) {
			return true
		}
	}
	return false
}
