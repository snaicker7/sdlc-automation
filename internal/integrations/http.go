// Package integrations holds thin REST clients for Atlassian (Jira + Confluence) and GitHub.
// Every integration is optional: a nil client means "use the local stand-in".
package integrations

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Set struct {
	Jira *Atlassian // Jira + Confluence share credentials
	GH   *GitHub
}

func (s *Set) Any() bool { return s != nil && (s.Jira != nil || s.GH != nil) }

// LoadDotEnv reads KEY=VALUE lines (ignoring comments) into the process env without
// overriding variables that are already set. Keeps secrets out of command lines.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// FromEnv builds the integration set from environment variables:
//
//	ATLASSIAN_SITE (https://yourco.atlassian.net)  ATLASSIAN_EMAIL  ATLASSIAN_API_TOKEN
//	JIRA_PROJECT (key)   CONFLUENCE_SPACE (key)
//	GITHUB_TOKEN  GITHUB_OWNER  [GITHUB_REPO_PREFIX, default "sdlc-"]
func FromEnv() *Set {
	s := &Set{}
	site, email, tok := os.Getenv("ATLASSIAN_SITE"), os.Getenv("ATLASSIAN_EMAIL"), os.Getenv("ATLASSIAN_API_TOKEN")
	if site != "" && email != "" && tok != "" {
		// Jira is used when JIRA_PROJECT is set, Confluence when CONFLUENCE_SPACE is set.
		s.Jira = &Atlassian{Site: strings.TrimRight(site, "/"), Email: email, Token: tok,
			Project: os.Getenv("JIRA_PROJECT"), Space: os.Getenv("CONFLUENCE_SPACE"), HC: client()}
	}
	if t, o := os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_OWNER"); t != "" && o != "" {
		p := os.Getenv("GITHUB_REPO_PREFIX")
		if p == "" {
			p = "sdlc-"
		}
		s.GH = &GitHub{Token: t, Owner: o, Prefix: p, HC: client(), APIBase: os.Getenv("GITHUB_API_URL"), GitBase: os.Getenv("GITHUB_GIT_BASE")}
	}
	return s
}

func (s *Set) JiraEnabled() bool { return s != nil && s.Jira != nil && s.Jira.Project != "" }
func (s *Set) ConfluenceEnabled() bool {
	return s != nil && s.Jira != nil && s.Jira.Space != ""
}

func client() *http.Client { return &http.Client{Timeout: 45 * time.Second} }

// doJSON performs a JSON request, retrying once on 429, and decodes the response into out.
func doJSON(ctx context.Context, hc *http.Client, method, url string, hdr map[string]string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 && attempt == 0 {
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			if wait <= 0 || wait > 30 {
				wait = 5
			}
			time.Sleep(time.Duration(wait) * time.Second)
			continue
		}
		if resp.StatusCode >= 300 {
			return &HTTPError{Status: resp.StatusCode, Method: method, URL: url, Body: truncate(string(data), 600)}
		}
		if out != nil && len(data) > 0 {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

type HTTPError struct {
	Status      int
	Method, URL string
	Body        string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, e.Body)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
