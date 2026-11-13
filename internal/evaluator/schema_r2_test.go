package evaluator

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSchemaRejectsDependencyOutsideApprovedInputSet(t *testing.T) {
	f := newFixture(t, true)
	f.write("oracle.json", `{"const":"expected"}`)
	ref, err := json.Marshal(map[string]string{"$ref": "file://" + filepath.ToSlash(filepath.Join(f.dir, "oracle.json"))})
	if err != nil {
		t.Fatal(err)
	}
	f.write("schema.json", string(ref))
	f.write("actual.json", `"wrong"`)
	spec := ResolveInputs(f.dir, []Spec{{ID: "schema", Kind: KindSchema, Schema: "schema.json", Files: []string{"actual.json"}, ApprovalEpoch: 1}})[0]
	f.write("oracle.json", `{}`)
	v := f.decide(spec)
	if v.Passed || v.Result.Status != StatusError {
		t.Fatalf("unapproved oracle relaxed a check: %+v", v)
	}
}

func TestSchemaLocalReferenceRemainsVersionBound(t *testing.T) {
	f := newFixture(t, true)
	f.write("oracle.json", `{"const":"expected"}`)
	f.write("schema.json", `{"$ref":"oracle.json"}`)
	f.write("actual.json", `"expected"`)
	spec := ResolveInputs(f.dir, []Spec{{ID: "schema", Kind: KindSchema, Schema: "schema.json", Files: []string{"actual.json"}, ApprovalEpoch: 1}})[0]
	if v := f.decide(spec); !v.Passed {
		t.Fatalf("approved local dependency: %+v", v)
	}
	f.write("oracle.json", `{}`)
	if v := f.decide(spec); v.Passed || v.Result.Status != StatusSpecUnapproved {
		t.Fatalf("changed local dependency still approved: %+v", v)
	}
}
