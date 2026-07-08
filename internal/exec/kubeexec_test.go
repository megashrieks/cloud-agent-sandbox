package exec

import (
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "''"},
		{name: "simple", in: "/tmp/file.txt", want: "'/tmp/file.txt'"},
		{name: "spaces", in: "/tmp/a file.txt", want: "'/tmp/a file.txt'"},
		{name: "single quote", in: "/tmp/bob's file", want: "'/tmp/bob'\\''s file'"},
		{name: "shell metacharacters", in: "; rm -rf /", want: "'; rm -rf /'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellQuote(tt.in); got != tt.want {
				t.Fatalf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCapBufferTruncatesAndReportsFullWrite(t *testing.T) {
	buf := newCapBuffer(5)
	if n, err := buf.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("first write n=%d err=%v", n, err)
	}
	if buf.Truncated() {
		t.Fatal("buffer should not be truncated at exact limit")
	}
	if n, err := buf.Write([]byte(" world")); err != nil || n != 6 {
		t.Fatalf("second write n=%d err=%v", n, err)
	}
	if got, want := buf.String(), "hello"; got != want {
		t.Fatalf("buffer contents = %q, want %q", got, want)
	}
	if !buf.Truncated() {
		t.Fatal("buffer should be marked truncated")
	}
}

func TestCapBufferPartialWrite(t *testing.T) {
	buf := newCapBuffer(8)
	if n, err := buf.Write([]byte("hello world")); err != nil || n != len("hello world") {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if got, want := buf.String(), "hello wo"; got != want {
		t.Fatalf("buffer contents = %q, want %q", got, want)
	}
	if !buf.Truncated() {
		t.Fatal("buffer should be marked truncated")
	}
}

func TestParsePID(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    int
		wantErr bool
	}{
		{name: "plain", out: "1234\n", want: 1234},
		{name: "leading noise", out: "started\n5678\n", want: 5678},
		{name: "empty", out: "\n", wantErr: true},
		{name: "invalid", out: "abc\n", wantErr: true},
		{name: "zero", out: "0\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePID(tt.out)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePID(%q) succeeded, want error", tt.out)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePID(%q) error: %v", tt.out, err)
			}
			if got != tt.want {
				t.Fatalf("parsePID(%q) = %d, want %d", tt.out, got, tt.want)
			}
		})
	}
}

func TestTailBufferKeepsLatestBytes(t *testing.T) {
	buf := newTailBuffer(10)
	if n, err := buf.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("first write n=%d err=%v", n, err)
	}
	if got, want := buf.String(), "hello"; got != want {
		t.Fatalf("tail = %q, want %q", got, want)
	}
	if buf.Truncated() {
		t.Fatal("tail should not be truncated before limit")
	}
	if n, err := buf.Write([]byte(" wonderful world")); err != nil || n != len(" wonderful world") {
		t.Fatalf("second write n=%d err=%v", n, err)
	}
	if got, want := buf.String(), "rful world"; got != want {
		t.Fatalf("tail = %q, want %q", got, want)
	}
	if !buf.Truncated() {
		t.Fatal("tail should be marked truncated")
	}
}

func TestTailBufferMultipleLargeWrites(t *testing.T) {
	buf := newTailBuffer(6)
	_, _ = buf.Write([]byte(strings.Repeat("a", 20)))
	_, _ = buf.Write([]byte("bcdefg"))
	if got, want := buf.String(), "bcdefg"; got != want {
		t.Fatalf("tail = %q, want %q", got, want)
	}
	if !buf.Truncated() {
		t.Fatal("tail should be marked truncated")
	}
}

func TestParseFindList(t *testing.T) {
	got := parseFindList("d 4096 app\nf 12 read me.txt\nmalformed\n")
	if len(got) != 2 {
		t.Fatalf("len(parseFindList) = %d, want 2", len(got))
	}
	if got[0].Name != "app" || !got[0].IsDir || got[0].Size != 4096 {
		t.Fatalf("first entry = %+v", got[0])
	}
	if got[1].Name != "read me.txt" || got[1].IsDir || got[1].Size != 12 {
		t.Fatalf("second entry = %+v", got[1])
	}
}
