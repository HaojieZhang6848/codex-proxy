package proxy

import "testing"

func TestRewriteModelPreservesEveryOtherByte(t *testing.T) {
	body := []byte(`{"model":"local","input":"hi","nested":{"model":"untouched"}," n":1}`)
	got, changed, err := rewriteModel(body, "remote-model")
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	if !changed {
		t.Fatal("expected the body to change")
	}
	want := `{"model":"remote-model","input":"hi","nested":{"model":"untouched"}," n":1}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestRewriteModelKeepsEverythingWhenUnchanged(t *testing.T) {
	body := []byte(`{"model":"same","input":"hi"}`)
	got, changed, err := rewriteModel(body, "same")
	if err != nil || changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	if string(got) != string(body) {
		t.Fatalf("body was copied unnecessarily")
	}
}

func TestModelNameOf(t *testing.T) {
	cases := []struct {
		body  string
		name  string
		found bool
	}{
		{`{"model":"a"}`, "a", true},
		{`{ "model" : "a\u0062" , "x": 1 }`, "ab", true},
		{`{"input":"hi"}`, "", false},
		{`[1,2,3]`, "", false},
		{``, "", false},
		{`not json`, "", false},
	}
	for _, tc := range cases {
		name, found := modelNameOf([]byte(tc.body))
		if name != tc.name || found != tc.found {
			t.Errorf("modelNameOf(%q) = %q, %v; want %q, %v", tc.body, name, found, tc.name, tc.found)
		}
	}
}

func TestRewriteModelRejectsNonStringModel(t *testing.T) {
	if _, _, err := rewriteModel([]byte(`{"model":{"a":1}}`), "x"); err == nil {
		t.Fatal("expected an error for a non string model field")
	}
	// null is replaced by the mapped name.
	got, changed, err := rewriteModel([]byte(`{"model":null,"x":1}`), "x")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if string(got) != `{"model":"x","x":1}` {
		t.Fatalf("got %s", got)
	}
}
