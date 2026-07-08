package mcp

import "testing"

func TestRenderNumberedLines(t *testing.T) {
	tests := []struct {
		name    string
		content string
		start   int
		end     int
		want    string
	}{
		{name: "all lines", content: "alpha\nbeta\ngamma", start: 0, end: 0, want: "1: alpha\n2: beta\n3: gamma"},
		{name: "range", content: "alpha\nbeta\ngamma\n", start: 2, end: 3, want: "2: beta\n3: gamma"},
		{name: "past end", content: "alpha\nbeta", start: 3, end: 0, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderNumberedLines(tt.content, tt.start, tt.end)
			if got != tt.want {
				t.Fatalf("renderNumberedLines() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReplaceUnique(t *testing.T) {
	tests := []struct {
		name    string
		content string
		oldStr  string
		newStr  string
		want    string
		wantErr string
	}{
		{name: "unique", content: "hello old world", oldStr: "old", newStr: "new", want: "hello new world"},
		{name: "zero", content: "hello world", oldStr: "old", newStr: "new", wantErr: "old_str not found"},
		{name: "multi", content: "old and old", oldStr: "old", newStr: "new", wantErr: "old_str is not unique (2 matches)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := replaceUnique(tt.content, tt.oldStr, tt.newStr)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("replaceUnique() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("replaceUnique() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("replaceUnique() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInsertAfterLine(t *testing.T) {
	tests := []struct {
		name    string
		content string
		line    int
		text    string
		want    string
	}{
		{name: "prepend", content: "one\ntwo\n", line: 0, text: "zero\n", want: "zero\none\ntwo\n"},
		{name: "middle", content: "one\nthree\n", line: 1, text: "two\n", want: "one\ntwo\nthree\n"},
		{name: "end", content: "one\ntwo", line: 2, text: "\nthree", want: "one\ntwo\nthree"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := insertAfterLine(tt.content, tt.line, tt.text)
			if err != nil {
				t.Fatalf("insertAfterLine() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("insertAfterLine() = %q, want %q", got, tt.want)
			}
		})
	}
}
