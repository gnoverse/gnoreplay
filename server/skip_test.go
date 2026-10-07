package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCannotAffectNode(t *testing.T) {
	for file, want := range map[string]bool{
		"docs/resources/effective-gno.md":                       true,
		"README.md":                                             true,
		"gno.land/adr/pr6025_prod_only_typecheck.md":            true,
		".github/workflows/gnovm.yml":                           true,
		"examples/gno.land/r/demo/boards/board.gno":             true,
		"contribs/gnodev/cmd/gnodev/main.go":                    true,
		"misc/deployments/gnoland1/genesis.json":                true,
		"gnovm/cmd/gno/test.go":                                 true,
		"gnovm/pkg/test/test.go":                                true,
		"gnovm/tests/files/for1.gno":                            true,
		"gno.land/pkg/gnoweb/app.go":                            true,
		"gno.land/pkg/integration/testdata/addpkg.txtar":        true,
		"gno.land/pkg/sdk/vm/keeper_test.go":                    true,
		"tm2/pkg/amino/testdata/x.json":                         true,
		".gitignore":                                            true,
		"gno.land/pkg/sdk/vm/keeper.go":                         false,
		"gnovm/pkg/gnolang/preprocess.go":                       false,
		"tm2/pkg/sdk/auth/ante.go":                              false,
		"go.mod":                                                false,
		"go.sum":                                                false,
		"Makefile":                                              false,
		"gno.land/cmd/gnoland/start.go":                         false,
		"gnovm/tests/stdlibs/testing/native_testing.go":         false, // linked into the node
		"gnovm/stdlibs/strings/strings.gno":                     false,
		"gnovm/stdlibs/strings/strings_test.gno":                false, // in the state
		"gnovm/stdlibs/strings/README.md":                       false,
		"gnovm/stdlibs/encoding/base64/base64_test.go":          false,
		"gnovm/stdlibs/encoding/base64/testdata/fuzz/corpus.go": false,
	} {
		assert.Equal(t, want, cannotAffectNode(file), file)
	}
}

func TestSkipReason(t *testing.T) {
	assert.Contains(t, skipReason([]string{"docs/a.md", "examples/gno.land/p/x/x.gno"}), "none of its 2 changed file(s)")
	assert.Empty(t, skipReason([]string{"docs/a.md", "gnovm/pkg/gnolang/op_call.go"}), "one relevant file")
	assert.Empty(t, skipReason(nil), "unknown files")
	many := make([]string, maxPullFiles)
	for i := range many {
		many[i] = "docs/a.md"
	}
	assert.Empty(t, skipReason(many), "GitHub's list may be truncated")
}
