package generator

import (
	"bytes"
	"go/build"
	"io"
	"strings"
)

// ports are the GOOS/GOARCH pairs Go builds for, as `go tool dist list` prints them on Go 1.26
// and Go 1.27.
var ports = strings.Fields(`aix/ppc64 android/386 android/amd64 android/arm android/arm64
darwin/amd64 darwin/arm64 dragonfly/amd64 freebsd/386 freebsd/amd64 freebsd/arm freebsd/arm64
illumos/amd64 ios/amd64 ios/arm64 js/wasm linux/386 linux/amd64 linux/arm linux/arm64
linux/loong64 linux/mips linux/mips64 linux/mips64le linux/mipsle linux/ppc64 linux/ppc64le
linux/riscv64 linux/s390x netbsd/386 netbsd/amd64 netbsd/arm netbsd/arm64 openbsd/386
openbsd/amd64 openbsd/arm openbsd/arm64 openbsd/ppc64 openbsd/riscv64 plan9/386 plan9/amd64
plan9/arm solaris/amd64 wasip1/wasm windows/386 windows/amd64 windows/arm64`)

// platformSet is a set of platforms, a bit per port with cgo off and one per port with cgo on.
type platformSet [2]uint64

// platformCount is the number of platforms a platformSet holds.
var platformCount = 2 * len(ports)

func (s *platformSet) add(i int) {
	s[i/64] |= 1 << (i % 64)
}

func (s platformSet) union(other platformSet) platformSet {
	return platformSet{s[0] | other[0], s[1] | other[1]}
}

// within reports whether every platform of s is in other.
func (s platformSet) within(other platformSet) bool {
	return s[0]&^other[0] == 0 && s[1]&^other[1] == 0
}

// toolTags are the GOEXPERIMENT tags files are selected with: those of the environment the
// generator runs in, which go/build reads at run time, except goexperiment.jsonv2, which follows
// the rules compiled into the generator (encodingJSONV2), so that a dependency's files are read as
// the toolchain that builds the generator builds them. The tags go/build sets for the GOARCH the
// generator runs on, such as amd64.v1, are left out, so a file that needs one is not read.
var toolTags = func() []string {
	var tags []string
	for _, tag := range build.Default.ToolTags {
		if strings.HasPrefix(tag, "goexperiment.") && tag != "goexperiment.jsonv2" {
			tags = append(tags, tag)
		}
	}
	if encodingJSONV2 {
		tags = append(tags, "goexperiment.jsonv2")
	}
	return tags
}()

// filePlatforms is the set of platforms whose build selects the file name of a module package
// in dir, holding src: its GOOS and GOARCH file-name suffixes and its //go:build line, read by
// go/build for each port with cgo off and on, with toolTags, the release tags of the toolchain
// the generator runs on and no custom tags. go/build reads only the file's header and does not
// require a valid package clause, so a file selected on no platform, such as a template marked
// //go:build ignore, is left out before it is parsed. Its error says why the constraints cannot
// be evaluated.
func filePlatforms(dir string, name string, src []byte) (platformSet, error) {
	var selected platformSet
	for i := range platformCount {
		goos, goarch, _ := strings.Cut(ports[i%len(ports)], "/")
		ctxt := build.Default
		ctxt.GOOS = goos
		ctxt.GOARCH = goarch
		ctxt.CgoEnabled = i >= len(ports)
		ctxt.BuildTags = nil
		ctxt.ToolTags = toolTags
		ctxt.OpenFile = func(string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(src)), nil
		}
		match, err := ctxt.MatchFile(dir, name)
		if err != nil {
			return platformSet{}, err
		}
		if match {
			selected.add(i)
		}
	}

	return selected, nil
}

// cgoPlatforms holds the platforms with cgo on, the only ones that build a file importing "C".
var cgoPlatforms = func() platformSet {
	var cgo platformSet
	for i := len(ports); i < platformCount; i++ {
		cgo.add(i)
	}
	return cgo
}()

func (s platformSet) intersect(other platformSet) platformSet {
	return platformSet{s[0] & other[0], s[1] & other[1]}
}
