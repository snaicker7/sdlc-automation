package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	rep := strings.Repeat
	tests := []struct {
		name    string
		in      Input
		want    Input
		wantErr bool
	}{
		{"trim title", Input{Title: "  hi  "}, Input{Title: "hi", Tags: []string{}}, false},
		{"empty title", Input{Title: ""}, Input{}, true},
		{"blank title", Input{Title: " \t\n "}, Input{}, true},
		{"200 runes", Input{Title: rep("é", 200)}, Input{Title: rep("é", 200), Tags: []string{}}, false},
		{"201 runes", Input{Title: rep("é", 201)}, Input{}, true},
		{"content 1MiB", Input{Title: "t", Content: rep("a", 1<<20)}, Input{Title: "t", Content: rep("a", 1<<20), Tags: []string{}}, false},
		{"content 1MiB+1", Input{Title: "t", Content: rep("a", 1<<20+1)}, Input{}, true},
		{"tag lowered", Input{Title: "t", Tags: []string{" Go "}}, Input{Title: "t", Tags: []string{"go"}}, false},
		{"tag comma", Input{Title: "t", Tags: []string{"a,b"}}, Input{}, true},
		{"tag 50", Input{Title: "t", Tags: []string{rep("a", 50)}}, Input{Title: "t", Tags: []string{rep("a", 50)}}, false},
		{"tag 51", Input{Title: "t", Tags: []string{rep("a", 51)}}, Input{}, true},
		{"tag empty", Input{Title: "t", Tags: []string{"  "}}, Input{}, true},
		{"dedupe", Input{Title: "t", Tags: []string{"b", "A", "B", "a", "c"}}, Input{Title: "t", Tags: []string{"b", "a", "c"}}, false},
		{"invalid utf8 content", Input{Title: "t", Content: "\xff"}, Input{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.in)
			if tt.wantErr {
				var ve *ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want *ValidationError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseFilter(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
		q    string
		qSet bool
	}{
		{"empty tag", []string{""}, "", false},
		{"blank tag", []string{"  "}, "", false},
		{"comma tag", []string{"a,b"}, "", false},
		{"blank q", nil, "  ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFilter(tc.tags, tc.q, tc.qSet)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
		})
	}
	f, err := ParseFilter([]string{" Go ", "WEB"}, "  HeLLo ", true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f, Filter{Tags: []string{"go", "web"}, Q: "hello"}) {
		t.Fatalf("got %+v", f)
	}
	// An unset q is ignored even when its value is blank.
	if _, err := ParseFilter(nil, "  ", false); err != nil {
		t.Fatal(err)
	}
}

func TestMatch(t *testing.T) {
	d := Document{ID: 1, Title: "Hello World", Content: "Some Body", Tags: []string{"go", "web"}}
	for _, tc := range []struct {
		name string
		f    Filter
		want bool
	}{
		{"empty", Filter{}, true},
		{"one tag", Filter{Tags: []string{"go"}}, true},
		{"tag case", Filter{Tags: []string{"GO"}}, true},
		{"tags and", Filter{Tags: []string{"go", "web"}}, true},
		{"tags and miss", Filter{Tags: []string{"go", "db"}}, false},
		{"tag exact not substring", Filter{Tags: []string{"g"}}, false},
		{"q title", Filter{Q: "hello"}, true},
		{"q content", Filter{Q: "BODY"}, true},
		{"q miss", Filter{Q: "zzz"}, false},
		{"tag and q", Filter{Tags: []string{"go"}, Q: "body"}, true},
		{"tag hit q miss", Filter{Tags: []string{"go"}, Q: "zzz"}, false},
		{"tag miss q hit", Filter{Tags: []string{"db"}, Q: "body"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(d, tc.f); got != tc.want {
				t.Fatalf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListOrdering(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	docs := []Document{
		{ID: 1, CreatedAt: t0},
		{ID: 2, CreatedAt: t0.Add(time.Hour)},
		{ID: 3, CreatedAt: t0},
		{ID: 4, CreatedAt: t0.Add(time.Hour)},
	}
	sortDocs(docs)
	var ids []int64
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	if !reflect.DeepEqual(ids, []int64{4, 2, 3, 1}) {
		t.Fatalf("order = %v", ids)
	}
}

func TestListEmptyIsNonNil(t *testing.T) {
	s, _ := openStore(t)
	docs, err := s.List(ctxBG, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(docs)
	if string(b) != "[]" {
		t.Fatalf("json = %s", b)
	}
}

func TestSummaryOmitsContent(t *testing.T) {
	d := Document{ID: 7, Title: "t", Content: "secret", CreatedAt: time.Now()}
	b, err := json.Marshal(d.Summary())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "content") || strings.Contains(string(b), "secret") {
		t.Fatalf("summary leaks content: %s", b)
	}
	if !strings.Contains(string(b), `"tags":[]`) {
		t.Fatalf("summary tags should be []: %s", b)
	}
}
