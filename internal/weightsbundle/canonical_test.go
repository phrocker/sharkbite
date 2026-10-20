// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Expected values come from CPython 3.12 json.dumps(sort_keys=True,
// separators=(',', ':')) and hashlib.sha256 over the same object.
func TestCanonicalMatchesPythonJSONDumps(t *testing.T) {
	value := map[string]any{
		"b": 1,
		"a": "xé\U0001F600<>&\"\\\n\u0001",
		"c": []any{true, nil, 0.5, 1.0, 1e16, 1e-5, 0.0001, int64(123456789012345678), 2.5e-7, 1e22, 100.0, 1234567890123456.0, 0.00001234, -2.0, 0.1},
		"d": map[string]any{"z": []any{}, "y": map[string]any{}},
	}
	want := `{"a":"x\u00e9\ud83d\ude00<>&\"\\\n\u0001","b":1,"c":[true,null,0.5,1.0,1e+16,1e-05,0.0001,123456789012345678,2.5e-07,1e+22,100.0,1234567890123456.0,1.234e-05,-2.0,0.1],"d":{"y":{},"z":[]}}`
	got, err := Canonical(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical mismatch\n got %s\nwant %s", got, want)
	}
	if digest := SHA256Hex(got); digest != "8ab0d88cfb7ba5caf9e04c4706d319d755c498764c1dc31f23a8d4f65c670043" {
		t.Fatalf("digest %s", digest)
	}
	// An empty key is an ordinary key, as in Python; only the record
	// identity is ever skipped, and only at the top level.
	if got, _ := Canonical(map[string]any{"": 8, "a": 2}); string(got) != `{"":8,"a":2}` {
		t.Fatalf("empty key dropped: %s", got)
	}
	a, _ := Seal("x", map[string]any{"": 1, "a": 2})
	b, _ := Seal("x", map[string]any{"a": 2})
	if a["id"] == b["id"] {
		t.Fatal("empty key does not change the identity")
	}
	nested, _ := Canonical(map[string]any{"id": 1, "x": map[string]any{"id": 2}})
	if string(nested) != `{"id":1,"x":{"id":2}}` {
		t.Fatalf("nested id dropped: %s", nested)
	}
	// Python parses -0 as 0; a literal that is not an integer is refused.
	if got, _ := Canonical(json.Number("-0")); string(got) != "0" {
		t.Fatalf("-0: %s", got)
	}
	for _, bad := range []string{"01", "+1", "0x10", "-"} {
		if _, err := Canonical(json.Number(bad)); err == nil {
			t.Fatalf("literal %q accepted", bad)
		}
	}
	// Records decoded from JSON keep their literal numbers.
	record, err := DecodeRecord(got)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Canonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != want {
		t.Fatalf("decoded record re-encodes differently: %s", again)
	}
}

func TestSealMatchesPythonLearningSeal(t *testing.T) {
	record, err := Seal("candidate", map[string]any{"dataset_id": "d", "evaluation_policy_id": "e", "training_ids": []string{"0"}, "model_digest": "model", "policy_predeclared": true})
	if err != nil {
		t.Fatal(err)
	}
	if record["id"] != "a70ec249008c87f03103b4435957e8a1686341f03ed659217edc29f54eb273aa" {
		t.Fatalf("id %v", record["id"])
	}
	if err := VerifyRecord(record, "candidate"); err != nil {
		t.Fatal(err)
	}
	record["model_digest"] = "other"
	if err := VerifyRecord(record, "candidate"); err == nil || !strings.Contains(err.Error(), "invalid candidate identity") {
		t.Fatalf("mutation accepted: %v", err)
	}
	if _, err := Seal("candidate", map[string]any{"id": "x"}); err == nil {
		t.Fatal("reserved field accepted")
	}
	if _, err := Seal("", nil); err == nil {
		t.Fatal("empty kind accepted")
	}
}

func TestPythonChainFixtureVerifies(t *testing.T) {
	chain := loadChain(t)
	for name, kind := range map[string]string{"candidate": KindCandidate, "evaluation": KindEvaluation, "artifact": KindArtifact, "approval": KindPromotionApproval, "first_release": KindRelease, "second_release": KindRelease, "rollback_approval": KindRollbackApproval, "rollback": KindRelease} {
		if err := VerifyRecord(chain[name], kind); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	manifest, err := DecodeManifest(mustCanonical(t, chain["manifest"]))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := manifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if want := stringFieldChain(chain, "bundle_digest"); digest != want {
		t.Fatalf("bundle digest %s, python %s", digest, want)
	}
}

func TestPythonFloatRepr(t *testing.T) {
	cases := map[float64]string{0: "0.0", 1: "1.0", 0.5: "0.5", 1e16: "1e+16", 1e15: "1000000000000000.0", 1e-5: "1e-05", 0.0001: "0.0001", 2.5e-7: "2.5e-07", 1e22: "1e+22", 1.5e300: "1.5e+300", 5e-324: "5e-324", -2: "-2.0", 1234567890123456: "1234567890123456.0", 12345678901234567: "1.2345678901234568e+16", 0.1: "0.1", 123.456: "123.456"}
	for f, want := range cases {
		if got := pythonFloatRepr(f); got != want {
			t.Errorf("%v: got %s want %s", f, got, want)
		}
	}
}

func TestDecodeRecordRejectsMalformedInput(t *testing.T) {
	for _, raw := range []string{`{"a":1,"a":2}`, `{"a":1} {}`, `null`, `[1]`, "{\"a\":\"\xff\"}", `{"a":`} {
		if _, err := DecodeRecord([]byte(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	if _, err := DecodeRecord([]byte(strings.Repeat(" ", int(MaxRecordBytes)+1))); err == nil {
		t.Error("accepted oversized record")
	}
}

type chain map[string]Record

func loadChain(t *testing.T) chain {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "python-chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	top, err := DecodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	out := chain{}
	for k, v := range top {
		if m, ok := v.(map[string]any); ok {
			out[k] = Record(m)
		} else {
			out[k] = Record{"value": v}
		}
	}
	out["bundle_digest"] = Record{"value": top["bundle_digest"]}
	return out
}

func (c chain) release(t *testing.T, name string) Release {
	t.Helper()
	r, err := ReleaseFromRecord(c[name])
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r
}

func stringFieldChain(c chain, name string) string { return stringField(c[name], "value") }

func mustCanonical(t *testing.T, v any) []byte {
	t.Helper()
	b, err := Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
