// Package store persists documents in a single JSON file with cross-process locking.
package store

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Validation limits.
const (
	MaxTitleRunes   = 200
	MaxContentBytes = 1 << 20
	MaxTagRunes     = 50
)

// Document is a stored document.
type Document struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

// Summary is a Document without its content.
type Summary struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

// Summary returns d without its content.
func (d Document) Summary() Summary {
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}
	return Summary{ID: d.ID, Title: d.Title, Tags: tags, CreatedAt: d.CreatedAt}
}

// Input is the user-supplied part of a document.
type Input struct {
	Title   string
	Content string
	Tags    []string
}

// ValidationError reports invalid user input.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Message)
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Normalize validates in and returns its canonical form.
func Normalize(in Input) (Input, error) {
	title := strings.TrimSpace(in.Title)
	switch n := utf8.RuneCountInString(title); {
	case n == 0:
		return Input{}, invalid("title", "must not be empty")
	case n > MaxTitleRunes:
		return Input{}, invalid("title", "must be at most %d characters", MaxTitleRunes)
	}
	if len(in.Content) > MaxContentBytes {
		return Input{}, invalid("content", "must be at most %d bytes", MaxContentBytes)
	}
	if !utf8.ValidString(in.Content) {
		return Input{}, invalid("content", "must be valid UTF-8")
	}
	tags := make([]string, 0, len(in.Tags))
	for _, raw := range in.Tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case tag == "":
			return Input{}, invalid("tag", "must not be empty")
		case strings.Contains(tag, ","):
			return Input{}, invalid("tag", "%q must not contain a comma", tag)
		case utf8.RuneCountInString(tag) > MaxTagRunes:
			return Input{}, invalid("tag", "must be at most %d characters", MaxTagRunes)
		}
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	return Input{Title: title, Content: in.Content, Tags: tags}, nil
}

// Filter selects documents: all Tags must be present and Q must appear in the title or content.
type Filter struct {
	Tags []string
	Q    string
}

// ParseFilter validates and canonicalises filter values; q is only checked when qSet is true.
func ParseFilter(tags []string, q string, qSet bool) (Filter, error) {
	var f Filter
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case tag == "":
			return Filter{}, invalid("tag", "must not be empty")
		case strings.Contains(tag, ","):
			return Filter{}, invalid("tag", "%q must not contain a comma", tag)
		}
		f.Tags = append(f.Tags, tag)
	}
	if qSet {
		f.Q = strings.ToLower(strings.TrimSpace(q))
		if f.Q == "" {
			return Filter{}, invalid("q", "must not be blank")
		}
	}
	return f, nil
}

// Match reports whether d satisfies f.
func Match(d Document, f Filter) bool {
	for _, want := range f.Tags {
		if !slices.ContainsFunc(d.Tags, func(t string) bool { return strings.EqualFold(t, want) }) {
			return false
		}
	}
	q := strings.ToLower(strings.TrimSpace(f.Q))
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(d.Title), q) || strings.Contains(strings.ToLower(d.Content), q)
}

// sortDocs orders by created_at descending, then id descending.
func sortDocs(docs []Document) {
	sort.Slice(docs, func(i, j int) bool {
		if !docs[i].CreatedAt.Equal(docs[j].CreatedAt) {
			return docs[i].CreatedAt.After(docs[j].CreatedAt)
		}
		return docs[i].ID > docs[j].ID
	})
}
