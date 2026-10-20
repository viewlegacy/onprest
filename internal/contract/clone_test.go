package contract

import (
	"bytes"
	"sync"
	"testing"
)

func TestCapabilityClonePreservesIndependentCompilerState(t *testing.T) {
	c := compile(t, wasm+`
input:
  properties:
    text: {type: string, pattern: '^a+$', default: aa}
    nested: {properties: {n: {type: integer}}}
    file: {type: file, extensions: [csv]}
output:
  properties:
    a: {type: string}
    b: {type: integer}
  data: {}
annotations: {read_only: false}
examples: [{params: {text: aa}}]
`, "")
	before := canonical(t, c.Normalized())
	clone := c.Clone()
	sameDefinition(t, c, clone)
	clone.Input.Properties["text"].Default = "bad"
	clone.Input.Properties["text"].Constraints["pattern"] = "bad"
	clone.Input.Properties["text"].normalized["default"] = "bad"
	clone.Input.Properties["text"].Pattern.AST.Kind = "bad"
	clone.Input.Properties["file"].File.Extensions[0] = "exe"
	clone.Input.PropertyOrder[0] = "bad"
	clone.Input.Properties["nested"].Properties["n"].Types[0] = "string"
	clone.Output.Schema.PropertyOrder[0] = "bad"
	clone.Executor.Settings["module"] = "bad"
	clone.Policy["timeout"] = "1ns"
	clone.Annotations["read_only"] = true
	clone.Examples[0]["params"].(map[string]any)["text"] = "bad"
	if !bytes.Equal(before, canonical(t, c.Normalized())) {
		t.Fatal("clone mutation changed original normalized definition")
	}
	if c.Input.PropertyOrder[0] != "text" || c.Output.Schema.PropertyOrder[0] != "a" || c.Input.Properties["file"].File.Extensions[0] != "csv" || !c.Input.Properties["text"].Pattern.MatchString("aaa") {
		t.Fatal("clone changed shared compiler state")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := c.Clone()
			local.Policy["timeout"] = "1s"
			if _, _, e := local.ValidateInput(map[string]any{}); e != nil {
				t.Error(e)
			}
			if !bytes.Equal(before, canonical(t, c.Normalized())) {
				t.Error("concurrent copy mutation")
			}
		}()
	}
	wg.Wait()
}
