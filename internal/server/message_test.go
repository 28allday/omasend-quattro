package server

import (
	"testing"

	"omasend/internal/protocol"
)

func TestMessageOf(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]protocol.FileMetadata
		wantOK  bool
		wantTxt string
	}{
		{
			name:    "text mime with preview is a message",
			files:   map[string]protocol.FileMetadata{"a": {FileType: "text/plain", Preview: "hi"}},
			wantOK:  true,
			wantTxt: "hi",
		},
		{
			name:   "bare text enum with preview is a message",
			files:  map[string]protocol.FileMetadata{"a": {FileType: "text", Preview: "yo"}},
			wantOK: true, wantTxt: "yo",
		},
		{
			name:   "text file without preview is a real file, not a message",
			files:  map[string]protocol.FileMetadata{"a": {FileType: "text/plain", Preview: ""}},
			wantOK: false,
		},
		{
			name:   "non-text with preview is not a message",
			files:  map[string]protocol.FileMetadata{"a": {FileType: "image/jpeg", Preview: "data"}},
			wantOK: false,
		},
		{
			name: "two files is never a message",
			files: map[string]protocol.FileMetadata{
				"a": {FileType: "text/plain", Preview: "hi"},
				"b": {FileType: "text/plain", Preview: "bye"},
			},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			txt, ok := messageOf(c.files)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && txt != c.wantTxt {
				t.Fatalf("text = %q, want %q", txt, c.wantTxt)
			}
		})
	}
}
