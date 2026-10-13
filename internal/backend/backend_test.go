package backend

import "testing"

func TestParseList(t *testing.T) {
	bs, err := ParseList(" http://a:8000, ,https://b:8001 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || bs[0].Name != "a:8000" || bs[1].Name != "b:8001" {
		t.Fatalf("got %+v", bs)
	}
}

func TestParseListRejects(t *testing.T) {
	for _, in := range []string{"", " , ", "localhost:8000", "ftp://a:21", "http://"} {
		if _, err := ParseList(in); err == nil {
			t.Errorf("ParseList(%q) succeeded, want error", in)
		}
	}
}
