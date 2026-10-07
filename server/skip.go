package main

import (
	"fmt"
	"path"
	"strings"
)

// A PR whose changes can't reach the node is not replayed. A replay builds the
// node's Go packages from the PR's tree and loads gnovm/stdlibs from it at
// runtime; anything else is irrelevant to it. Pushes to branches are always
// replayed, whatever they change.

// noNodeDirs are trees that neither the gnoland binary nor the replay tool
// links or reads (per `go list -deps ./gno.land/cmd/gnoland` and of the replay
// tool, on master in October 2026): documentation, CI, examples, and tools.
// A PR that makes the node depend on one of them also changes the node's own
// files, so it is replayed.
var noNodeDirs = []string{
	"docs/", ".github/", "examples/", "contribs/", "misc/",
	"gno.land/cmd/gnokey/", "gno.land/cmd/gnoweb/",
	"gno.land/pkg/gnoclient/", "gno.land/pkg/gnoweb/", "gno.land/pkg/integration/", "gno.land/pkg/keyscli/",
	"gnovm/cmd/", "gnovm/pkg/gnofmt/", "gnovm/pkg/integration/", "gnovm/pkg/repl/", "gnovm/pkg/test/",
	"gnovm/pkg/transpiler/", "gnovm/tests/files/",
	"tm2/pkg/testutils/",
}

var noNodeNames = map[string]bool{
	".gitignore": true, ".gitattributes": true, ".dockerignore": true, ".editorconfig": true,
	"CODEOWNERS": true, "LICENSE": true,
}

// cannotAffectNode reports whether a changed file is irrelevant to a replay.
func cannotAffectNode(file string) bool {
	// The stdlibs are loaded into the state whole, their tests and READMEs
	// included: any change there changes the app hash.
	if strings.HasPrefix(file, "gnovm/stdlibs/") {
		return false
	}
	switch {
	case strings.HasSuffix(file, ".md"), strings.HasSuffix(file, "_test.go"), strings.HasSuffix(file, ".txtar"),
		noNodeNames[path.Base(file)], strings.Contains("/"+file, "/testdata/"):
		return true
	}
	for _, dir := range noNodeDirs {
		if strings.HasPrefix(file, dir) {
			return true
		}
	}
	return false
}

// maxPullFiles is how many files GitHub lists for a PR at most: a PR with
// more can't be judged from its list.
const maxPullFiles = 3000

// skipReason says why a PR changing files needs no replay, or returns "" if
// it needs one.
func skipReason(files []string) string {
	if len(files) == 0 || len(files) >= maxPullFiles {
		return ""
	}
	for _, f := range files {
		if !cannotAffectNode(f) {
			return ""
		}
	}
	return fmt.Sprintf("none of its %d changed file(s) can affect the node (documentation, tests, CI, examples or tools)", len(files))
}
