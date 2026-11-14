package scraper

import (
	"strings"
	"testing"
)

func TestTemplate(t *testing.T) {
	tests := []struct {
		name    string
		isURL   bool
		str     string
		vars    map[string]any
		want    string
		wantErr string
	}{
		{
			name: "basic substitution",
			str:  "hello {{name}}",
			vars: map[string]any{"name": "world"},
			want: "hello world",
		},
		{
			name: "default value when var missing",
			str:  "hi {{name:stranger}}",
			vars: map[string]any{},
			want: "hi stranger",
		},
		{
			name: "explicit value beats default",
			str:  "hi {{name:stranger}}",
			vars: map[string]any{"name": "alice"},
			want: "hi alice",
		},
		{
			name:    "missing required var",
			str:     "hi {{name}}",
			vars:    map[string]any{},
			wantErr: "missing param: name",
		},
		{
			name:  "url-escape after question mark",
			isURL: true,
			str:   "https://example.com/search?q={{q}}",
			vars:  map[string]any{"q": "hello world"},
			want:  "https://example.com/search?q=hello+world",
		},
		{
			name:  "no escape before question mark",
			isURL: true,
			str:   "https://example.com/{{path}}?q=x",
			vars:  map[string]any{"path": "a/b"},
			want:  "https://example.com/a/b?q=x",
		},
		{
			name: "first missing param wins",
			str:  "{{a}} and {{b}}",
			vars: map[string]any{},
			// no value substituted on error
			wantErr: "missing param: a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := template(tt.isURL, tt.str, tt.vars)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				if got != "" {
					t.Fatalf("want empty output on error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckSelector(t *testing.T) {
	if err := checkSelector("div.foo > a[href]"); err != nil {
		t.Errorf("valid selector rejected: %v", err)
	}
	// goquery is permissive but should at least not panic on garbage
	_ = checkSelector("[[[")
}
