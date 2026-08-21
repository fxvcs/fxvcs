package canonical

import (
	"testing"
)

func TestMarshalSortsKeysAndOmitsWhitespace(t *testing.T) {
	in := map[string]any{"b": 1, "a": []any{true, nil, "x\n\"y\""}, "é": map[string]any{"z": int64(0), "y": "u"}}
	got, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":[true,null,"x\n\"y\""],"b":1,"é":{"y":"u","z":0}}`
	if string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestMarshalIsDeterministicAcrossInsertionOrder(t *testing.T) {
	a := map[string]any{"k1": "v", "k2": "w", "k3": []any{int64(1), int64(2)}}
	b := map[string]any{"k3": []any{int64(1), int64(2)}, "k2": "w", "k1": "v"}
	da, _ := Digest(a)
	db, _ := Digest(b)
	if da != db {
		t.Fatalf("digests differ: %s %s", da, db)
	}
}

func TestMarshalRejectsFloatsAndLargeInts(t *testing.T) {
	if _, err := Marshal(map[string]any{"f": 1.5}); err == nil {
		t.Fatal("expected float rejection")
	}
	if _, err := Marshal(map[string]any{"i": int64(1 << 60)}); err == nil {
		t.Fatal("expected large int rejection")
	}
	if _, err := Marshal("\xff"); err == nil {
		t.Fatal("expected invalid utf-8 rejection")
	}
}

func TestStructRoundTripUsesJSONTags(t *testing.T) {
	type doc struct {
		Z string `json:"z"`
		A int    `json:"a"`
		O string `json:"o,omitempty"`
	}
	got, err := Marshal(doc{Z: "z", A: 3})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":3,"z":"z"}` {
		t.Fatalf("got %s", got)
	}
}

func TestRecanonicalizeRejectsDuplicateKeysAndTrailingData(t *testing.T) {
	if _, err := Recanonicalize([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("expected duplicate key rejection")
	}
	if _, err := Recanonicalize([]byte(`{"a":1} {}`)); err == nil {
		t.Fatal("expected trailing data rejection")
	}
	got, err := Recanonicalize([]byte("{ \"b\" : 2 ,\n \"a\" : [ ] }"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":[],"b":2}` {
		t.Fatalf("got %s", got)
	}
}

func TestUTF16Ordering(t *testing.T) {
	// U+1D11E (surrogate pair D834 DD1E) sorts before U+FF21 (FF21) under UTF-16
	// but after it under code-point/UTF-8 ordering.
	got, err := Marshal(map[string]any{"\U0001D11E": 1, "Ａ": 2})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"𝄞":1,"Ａ":2}` {
		t.Fatalf("got %s", got)
	}
}
