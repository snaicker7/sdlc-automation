// Package cli implements the docs shell subcommands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"docs/internal/store"
)

const usage = `usage: docs <command> [flags]

commands:
  add     --title TITLE [--tag TAG]... [--file PATH|-]   add a document; content comes from --file or stdin
  list    [--tag TAG]... [--q TEXT] [--summary]          list documents, newest first
  search  [--tag TAG]... [--q TEXT] [--summary]          like list, but needs --tag or --q
  get     ID                                             print one document
  delete  ID                                             delete one document
  serve   [--addr HOST:PORT]                             serve the JSON API on loopback (default 127.0.0.1:8080)
  help                                               show this help

every command accepts:
  --data PATH             data file (default $DOCS_FILE, then ./docs.json)
  --lock-timeout DURATION how long to wait for the file lock (default 5s)
`

const defaultDataFile = "./docs.json"

// usageError is a command-line misuse, reported with exit code 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{msg: fmt.Sprintf(format, args...)} }

// exitCode maps an error to a process exit code: 2 for bad input or usage, 1 for everything else.
func exitCode(err error) int {
	var ve *store.ValidationError
	var ue *usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ve), errors.As(err, &ue):
		return 2
	default:
		return 1
	}
}

// Run executes one docs command and returns the process exit code.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "docs: missing subcommand")
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(stderr, usage)
		return 0
	case "add":
		err = runAdd(ctx, rest, stdin, stdout)
	case "list":
		err = runList(ctx, "list", false, rest, stdout)
	case "search":
		err = runList(ctx, "search", true, rest, stdout)
	case "get":
		err = runGet(ctx, rest, stdout)
	case "delete":
		err = runDelete(ctx, rest)
	case "serve":
		err = runServe(ctx, rest, stderr)
	default:
		fmt.Fprintf(stderr, "docs: unknown subcommand %q\n", cmd)
		fmt.Fprint(stderr, usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stderr, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "docs: %s\n", oneLine(err.Error()))
	}
	return exitCode(err)
}

func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// common holds the flags every subcommand accepts.
type common struct {
	data        string
	lockTimeout time.Duration
}

func addCommonFlags(fs *flag.FlagSet, c *common) {
	fs.StringVar(&c.data, "data", "", "data file")
	fs.DurationVar(&c.lockTimeout, "lock-timeout", 5*time.Second, "lock wait")
}

func newFlagSet(name string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := &common{}
	addCommonFlags(fs, c)
	return fs, c
}

// parse parses flags that may be interleaved with positional arguments and returns the latter.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &usageError{msg: err.Error()}
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// resolvePath applies the precedence --data, then $DOCS_FILE, then ./docs.json.
func resolvePath(flagValue string, getenv func(string) string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := getenv("DOCS_FILE"); v != "" {
		return v
	}
	return defaultDataFile
}

func openStore(ctx context.Context, c *common) (*store.Store, error) {
	return store.Open(ctx, store.Options{Path: resolvePath(c.data, os.Getenv), LockTimeout: c.lockTimeout})
}

// tagList is a repeatable --tag flag.
type tagList []string

func (t *tagList) String() string     { return strings.Join(*t, ",") }
func (t *tagList) Set(v string) error { *t = append(*t, v); return nil }

func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", b); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// readContent reads at most store.MaxContentBytes from path (or stdin when path is empty or "-").
func readContent(ctx context.Context, path string, stdin io.Reader) (content string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r := stdin
	if path != "" && path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open content file: %w", err)
		}
		defer func() {
			if cerr := f.Close(); cerr != nil {
				err = errors.Join(err, fmt.Errorf("close content file: %w", cerr))
			}
		}()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, store.MaxContentBytes+1))
	if err != nil {
		return "", fmt.Errorf("read content: %w", err)
	}
	if len(b) > store.MaxContentBytes {
		return "", &store.ValidationError{Field: "content", Message: fmt.Sprintf("must be at most %d bytes", store.MaxContentBytes)}
	}
	return string(b), nil
}

func runAdd(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs, c := newFlagSet("add")
	var tags tagList
	title := fs.String("title", "", "document title")
	file := fs.String("file", "", "content file, or - for stdin")
	fs.Var(&tags, "tag", "tag (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("unexpected argument %q", pos[0])
	}
	if *title == "" {
		return usagef("add requires --title")
	}
	content, err := readContent(ctx, *file, stdin)
	if err != nil {
		return err
	}
	in, err := store.Normalize(store.Input{Title: *title, Content: content, Tags: tags})
	if err != nil {
		return err
	}
	s, err := openStore(ctx, c)
	if err != nil {
		return err
	}
	doc, err := s.Add(ctx, in)
	if err != nil {
		return err
	}
	return writeJSON(stdout, doc)
}

func runList(ctx context.Context, name string, requireFilter bool, args []string, stdout io.Writer) error {
	fs, c := newFlagSet(name)
	var tags tagList
	q := fs.String("q", "", "case-insensitive substring of title or content")
	summary := fs.Bool("summary", false, "omit content")
	fs.Var(&tags, "tag", "tag (repeatable, all must match)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("unexpected argument %q", pos[0])
	}
	qSet := false
	fs.Visit(func(f *flag.Flag) { qSet = qSet || f.Name == "q" })
	if requireFilter && len(tags) == 0 && !qSet {
		return usagef("%s requires at least one of --tag or --q", name)
	}
	filter, err := store.ParseFilter(tags, *q, qSet)
	if err != nil {
		return err
	}
	s, err := openStore(ctx, c)
	if err != nil {
		return err
	}
	docs, err := s.List(ctx, filter)
	if err != nil {
		return err
	}
	if !*summary {
		return writeJSON(stdout, docs)
	}
	sums := make([]store.Summary, 0, len(docs))
	for _, d := range docs {
		sums = append(sums, d.Summary())
	}
	return writeJSON(stdout, sums)
}

// parseIDArg parses the single positional ID argument.
func parseIDArg(name string, fs *flag.FlagSet, args []string) (int64, error) {
	pos, err := parse(fs, args)
	if err != nil {
		return 0, err
	}
	if len(pos) != 1 {
		return 0, usagef("%s requires exactly one ID", name)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil {
		return 0, &store.ValidationError{Field: "id", Message: fmt.Sprintf("%q is not a number", pos[0])}
	}
	return id, nil
}

func runGet(ctx context.Context, args []string, stdout io.Writer) error {
	fs, c := newFlagSet("get")
	id, err := parseIDArg("get", fs, args)
	if err != nil {
		return err
	}
	s, err := openStore(ctx, c)
	if err != nil {
		return err
	}
	doc, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	return writeJSON(stdout, doc)
}

func runDelete(ctx context.Context, args []string) error {
	fs, c := newFlagSet("delete")
	id, err := parseIDArg("delete", fs, args)
	if err != nil {
		return err
	}
	s, err := openStore(ctx, c)
	if err != nil {
		return err
	}
	return s.Delete(ctx, id)
}
