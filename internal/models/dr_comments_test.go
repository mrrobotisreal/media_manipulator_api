package models

import (
	"strings"
	"testing"
)

func TestDrCommentAnchorValidate(t *testing.T) {
	ptr := func(i int) *int { return &i }
	cases := []struct {
		name    string
		anchor  DrCommentAnchor
		wantErr bool
	}{
		{"valid text", DrCommentAnchor{Type: "text", BlockIndex: 0, Start: ptr(0), End: ptr(5), Quote: "hello"}, false},
		{"valid block", DrCommentAnchor{Type: "block", BlockIndex: 3}, false},
		{"text start == end", DrCommentAnchor{Type: "text", BlockIndex: 0, Start: ptr(5), End: ptr(5)}, true},
		{"text start > end", DrCommentAnchor{Type: "text", BlockIndex: 0, Start: ptr(9), End: ptr(2)}, true},
		{"text missing offsets", DrCommentAnchor{Type: "text", BlockIndex: 0}, true},
		{"text negative offset", DrCommentAnchor{Type: "text", BlockIndex: 0, Start: ptr(-1), End: ptr(5)}, true},
		{"negative blockIndex", DrCommentAnchor{Type: "block", BlockIndex: -1}, true},
		{"unknown type", DrCommentAnchor{Type: "region", BlockIndex: 0}, true},
		{"quote too long", DrCommentAnchor{Type: "text", BlockIndex: 0, Start: ptr(0), End: ptr(1), Quote: strings.Repeat("x", drAnchorMaxQuote+1)}, true},
		{"quote at limit", DrCommentAnchor{Type: "text", BlockIndex: 0, Start: ptr(0), End: ptr(1), Quote: strings.Repeat("x", drAnchorMaxQuote)}, false},
	}
	for _, tc := range cases {
		if err := tc.anchor.Validate(); (err != nil) != tc.wantErr {
			t.Errorf("%s: Validate() err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}
