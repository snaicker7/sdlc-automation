package integrations

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Atlassian talks to Jira Cloud (REST v3) and Confluence Cloud (REST v2) with an API token.
type Atlassian struct {
	Site, Email, Token, Project, Space string
	HC                                 *http.Client
}

func (a *Atlassian) hdr() map[string]string {
	return map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(a.Email+":"+a.Token))}
}

func (a *Atlassian) call(ctx context.Context, method, path string, in, out any) error {
	return doJSON(ctx, a.HC, method, a.Site+path, a.hdr(), in, out)
}

// ---------------------------------------------------------------- Jira

// Block is an Atlassian Document Format node (Jira v3 descriptions must be ADF).
type Block map[string]any

func text(s string) []Block { return []Block{{"type": "text", "text": s}} }

func P(s string) Block {
	if strings.TrimSpace(s) == "" {
		s = " "
	}
	return Block{"type": "paragraph", "content": text(s)}
}

func H(level int, s string) Block {
	return Block{"type": "heading", "attrs": map[string]any{"level": level}, "content": text(s)}
}

func UL(items []string) Block {
	var li []Block
	for _, it := range items {
		li = append(li, Block{"type": "listItem", "content": []Block{P(it)}})
	}
	return Block{"type": "bulletList", "content": li}
}

type IssueIn struct {
	Type, Summary, Parent string
	Description           []Block
	Labels                []string
}

type IssueOut struct{ Key, URL string }

func (a *Atlassian) CreateIssue(ctx context.Context, in IssueIn) (IssueOut, error) {
	fields := map[string]any{
		"project":   map[string]any{"key": a.Project},
		"summary":   in.Summary,
		"issuetype": map[string]any{"name": in.Type},
	}
	if len(in.Description) > 0 {
		fields["description"] = map[string]any{"type": "doc", "version": 1, "content": in.Description}
	}
	if in.Parent != "" {
		fields["parent"] = map[string]any{"key": in.Parent}
	}
	if len(in.Labels) > 0 {
		fields["labels"] = in.Labels
	}
	var out struct{ Key string }
	if err := a.call(ctx, "POST", "/rest/api/3/issue", map[string]any{"fields": fields}, &out); err != nil {
		return IssueOut{}, err
	}
	return IssueOut{Key: out.Key, URL: a.Site + "/browse/" + out.Key}, nil
}

// Transition moves an issue to the first reachable status whose name matches one of names
// (case-insensitive). Returns false (no error) if none is reachable, since workflows differ.
func (a *Atlassian) Transition(ctx context.Context, key string, names ...string) (bool, error) {
	var tr struct {
		Transitions []struct {
			ID, Name string
			To       struct{ Name string }
		}
	}
	if err := a.call(ctx, "GET", "/rest/api/3/issue/"+key+"/transitions", nil, &tr); err != nil {
		return false, err
	}
	for _, want := range names {
		for _, t := range tr.Transitions {
			if strings.EqualFold(t.Name, want) || strings.EqualFold(t.To.Name, want) {
				return true, a.call(ctx, "POST", "/rest/api/3/issue/"+key+"/transitions",
					map[string]any{"transition": map[string]any{"id": t.ID}}, nil)
			}
		}
	}
	return false, nil
}

// ---------------------------------------------------------------- Confluence

func (a *Atlassian) spaceID(ctx context.Context) (string, error) {
	var out struct{ Results []struct{ ID string } }
	if err := a.call(ctx, "GET", "/wiki/api/v2/spaces?keys="+url.QueryEscape(a.Space), nil, &out); err != nil {
		return "", err
	}
	if len(out.Results) == 0 {
		return "", fmt.Errorf("confluence space %q not found (or no permission)", a.Space)
	}
	return out.Results[0].ID, nil
}

// UpsertPage creates a page, or updates it when pageID is known. Returns the page id and web URL.
func (a *Atlassian) UpsertPage(ctx context.Context, pageID, title, storage string) (string, string, error) {
	body := map[string]any{"representation": "storage", "value": storage}
	var out struct {
		ID    string
		Links struct{ Base, Webui string } `json:"_links"`
	}
	if pageID == "" {
		sid, err := a.spaceID(ctx)
		if err != nil {
			return "", "", err
		}
		err = a.call(ctx, "POST", "/wiki/api/v2/pages", map[string]any{"spaceId": sid, "status": "current", "title": title, "body": body}, &out)
		if err != nil {
			return "", "", err
		}
	} else {
		var cur struct{ Version struct{ Number int } }
		if err := a.call(ctx, "GET", "/wiki/api/v2/pages/"+pageID, nil, &cur); err != nil {
			return "", "", err
		}
		err := a.call(ctx, "PUT", "/wiki/api/v2/pages/"+pageID, map[string]any{"id": pageID, "status": "current", "title": title,
			"body": body, "version": map[string]any{"number": cur.Version.Number + 1}}, &out)
		if err != nil {
			return "", "", err
		}
	}
	base := out.Links.Base
	if base == "" {
		base = a.Site + "/wiki"
	}
	return out.ID, base + out.Links.Webui, nil
}

var (
	boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)
	codeRe = regexp.MustCompile("`([^`]+)`")
	numRe  = regexp.MustCompile(`^\d+\.\s+`)
)

// MarkdownToStorage converts the Markdown our agents write into Confluence storage format (XHTML).
func MarkdownToStorage(src string) string {
	var sb strings.Builder
	list, inCode := "", false
	var code strings.Builder
	closeList := func() {
		if list != "" {
			sb.WriteString("</" + list + ">")
			list = ""
		}
	}
	inline := func(s string) string {
		s = html.EscapeString(s)
		s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
		return codeRe.ReplaceAllString(s, "<code>$1</code>")
	}
	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "```"):
			closeList()
			if inCode {
				sb.WriteString(`<ac:structured-macro ac:name="code"><ac:plain-text-body><![CDATA[` +
					strings.ReplaceAll(code.String(), "]]>", "]]]]><![CDATA[>") + `]]></ac:plain-text-body></ac:structured-macro>`)
				code.Reset()
			}
			inCode = !inCode
		case inCode:
			code.WriteString(line + "\n")
		case t == "":
			closeList()
		case strings.HasPrefix(t, "#"):
			closeList()
			lvl := len(t) - len(strings.TrimLeft(t, "#"))
			if lvl > 6 {
				lvl = 6
			}
			fmt.Fprintf(&sb, "<h%d>%s</h%d>", lvl, inline(strings.TrimSpace(t[lvl:])), lvl)
		case strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* "):
			if list != "ul" {
				closeList()
				sb.WriteString("<ul>")
				list = "ul"
			}
			sb.WriteString("<li>" + inline(t[2:]) + "</li>")
		case numRe.MatchString(t):
			if list != "ol" {
				closeList()
				sb.WriteString("<ol>")
				list = "ol"
			}
			sb.WriteString("<li>" + inline(numRe.ReplaceAllString(t, "")) + "</li>")
		default:
			closeList()
			sb.WriteString("<p>" + inline(t) + "</p>")
		}
	}
	closeList()
	return sb.String()
}

// Check verifies credentials and the configured project/space without creating anything.
func (a *Atlassian) Check(ctx context.Context) []string {
	var rep []string
	var me struct{ DisplayName, EmailAddress string }
	if err := a.call(ctx, "GET", "/rest/api/3/myself", nil, &me); err != nil {
		return []string{"FAIL  Atlassian auth: " + err.Error()}
	}
	rep = append(rep, "ok    Atlassian auth as "+me.DisplayName)
	if a.Project != "" {
		var pr struct {
			Name       string
			IssueTypes []struct{ Name string } `json:"issueTypes"`
		}
		if err := a.call(ctx, "GET", "/rest/api/3/project/"+url.PathEscape(a.Project)+"?expand=issueTypes", nil, &pr); err != nil {
			rep = append(rep, "FAIL  Jira project "+a.Project+": "+err.Error())
		} else {
			var names []string
			for _, t := range pr.IssueTypes {
				names = append(names, t.Name)
			}
			rep = append(rep, fmt.Sprintf("ok    Jira project %s (%s); issue types: %s", a.Project, pr.Name, strings.Join(names, ", ")))
			has := func(n string) bool {
				for _, x := range names {
					if strings.EqualFold(x, n) {
						return true
					}
				}
				return false
			}
			if !has("Epic") || !has("Story") {
				rep = append(rep, "WARN  project lacks an 'Epic' and/or 'Story' issue type; issue creation will fail until it has both")
			}
		}
	} else {
		rep = append(rep, "skip  JIRA_PROJECT not set: Jira stays local")
	}
	if a.Space != "" {
		if _, err := a.spaceID(ctx); err != nil {
			rep = append(rep, "FAIL  Confluence: "+err.Error())
		} else {
			rep = append(rep, "ok    Confluence space "+a.Space)
		}
	} else {
		rep = append(rep, "skip  CONFLUENCE_SPACE not set: documents stay local")
	}
	return rep
}

var ErrNotConfigured = errors.New("integration not configured")
