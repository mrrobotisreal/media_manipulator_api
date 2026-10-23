package metadata

import (
	"encoding/json"
	"testing"
)

func TestIDAllocatorDuplicates(t *testing.T) {
	a := newIDAllocator()
	got := []string{a.next("IFD0", "ModifyDate"), a.next("XMP-xmp", "ModifyDate"), a.next("IFD0", "ModifyDate"), a.next("IFD0", "ModifyDate")}
	want := []string{"IFD0:ModifyDate", "XMP-xmp:ModifyDate", "IFD0:ModifyDate#2", "IFD0:ModifyDate#3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("id %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOrderedMapKeepsOrder(t *testing.T) {
	b, err := json.Marshal(OrderedMap{{"z", 1}, {"a", "x"}, {"m", OrderedMap{{"q", true}}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"z":1,"a":"x","m":{"q":true}}` {
		t.Fatalf("got %s", b)
	}
}
