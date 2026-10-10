package web

const indexPage = `{{define "index"}}{{template "head" .}}<main>
<h1>Runs</h1><p class="sub">Each run takes an idea through the full pipeline. Agents do the work; people answer questions and approve at gates.</p>
<details class="card" {{if not .Runs}}open{{end}}><summary>＋ Start a new run</summary>
<form method="post" action="/start">
<p class="small mute" style="margin:6px 0 4px">Title</p><input type="text" name="title" placeholder="e.g. Document organizer" style="width:100%" required>
<p class="small mute" style="margin:10px 0 4px">What should be built? (the more concrete, the fewer questions the agents ask)</p>
<textarea name="idea" placeholder="Describe the feature, who it is for, and any constraints…" required></textarea>
<div class="form-row" style="display:flex;gap:10px;margin-top:10px;align-items:center">
<select name="size"><option value="standard">Standard change (PRD + design)</option><option value="trivial">Trivial change (skip PRD + design)</option><option value="large">Large change</option></select>
<input type="text" name="user" value="web-user" style="max-width:160px" title="your name for the audit trail">
<button class="primary">Start run</button></div></form></details>
{{range .Runs}}<a class="cardlink" href="/run/{{.Run.ID}}"><div class="card {{if .Needs}}attn{{end}}">
<div class="row"><div><h3>{{.Run.Title}}</h3><div class="sub small">{{slice .Run.ID 0 8}} · {{.Run.Team}} · ${{printf "%.2f" .Run.SpentUSD}} of ${{printf "%.2f" .Run.BudgetUSD}}</div></div>
<div>{{if .Needs}}<span class="pill st-awaiting_human">{{.Needs}} waiting for you</span> {{end}}<span class="pill st-{{.Run.Status}}">{{.Run.Status}}</span></div></div>
<div class="bar" style="margin-top:12px"><i style="width:{{pct .Done .Total}}%"></i></div><div class="small mute" style="margin-top:4px">{{.Done}} of {{.Total}} phases done</div></div></a>
{{else}}<div class="empty">No runs yet. Start one above.</div>{{end}}
</main>{{template "foot" .}}{{end}}`

const runPage = `{{define "run"}}{{template "head" .}}<main>
{{$v := .V}}
<div class="small"><a href="/">← All runs</a></div>
<div class="titlebar"><h1>{{$v.Run.Title}}</h1><span class="pill st-{{$v.Run.Status}}">{{$v.Run.Status}}</span></div>
<p class="sub">{{$v.Run.Idea}}</p>
<div class="tiles">
<div class="tile"><b>{{doneCount $v}} / {{len $v.Phases}}</b><span>phases done</span><div class="bar" style="margin-top:8px"><i style="width:{{pct (doneCount $v) (len $v.Phases)}}%"></i></div></div>
<div class="tile"><b>{{merged $v}} / {{len (stories $v)}}</b><span>tickets merged</span></div>
<div class="tile"><b>${{printf "%.2f" $v.Run.SpentUSD}}</b><span>of ${{printf "%.0f" $v.Run.BudgetUSD}} budget</span></div>
<div class="tile"><b>{{$v.Agents}}</b><span>agent calls</span></div></div>
<nav class="tabs">
<a class="{{if eq .Tab "overview"}}on{{end}}" href="?tab=overview">Overview{{if .Attn}}<span class="badge">{{.Attn}}</span>{{end}}</a>
<a class="{{if eq .Tab "documents"}}on{{end}}" href="?tab=documents">Documents</a>
<a class="{{if eq .Tab "tickets"}}on{{end}}" href="?tab=tickets">Tickets &amp; MRs</a>
<a class="{{if eq .Tab "activity"}}on{{end}}" href="?tab=activity">Activity</a></nav>

{{if eq .Tab "overview"}}
{{range $v.Phases}}{{if .LastError}}<div class="card bad"><h3>⚠ {{label .Phase}} {{.Scope}} is blocked</h3><p class="small mute">An automatic step failed after retries. Fix the cause, then run <code>sdlc retry {{.ID}}</code>.</p><pre class="doc">{{.LastError}}</pre></div>{{end}}{{end}}

{{range $v.Phases}}{{if and (eq .Status "awaiting_human") (eq .Step "publish")}}{{$u := link $v "pr" .Scope}}<div class="card attn"><span class="tag">Waiting for you on GitHub</span><h3>Merge the pull request for {{.Scope}}</h3>
<p class="small mute" style="margin:4px 0 8px">The review gate is approved. Review and merge the PR on GitHub; the pipeline checks every 20 seconds and continues on its own once it is merged.</p>{{if $u}}<a href="{{$u}}" target="_blank" rel="noopener"><button class="primary">Open pull request ↗</button></a>{{end}}</div>{{end}}{{end}}

{{if .Attn}}<h2>Needs your attention</h2>
{{$oq := openQs $v}}{{if $oq}}<div class="card attn"><h3>Open questions ({{len $oq}})</h3>
<p class="small mute" style="margin:2px 0 6px">The agent will not proceed until each one is answered, or you explicitly accept its suggested default as an assumption (which is then written into the document).</p>
{{range $oq}}<div class="q"><span class="tag">{{label .Phase}} · {{prio .Priority}}</span><div><b>{{.Text}}</b></div>
{{if .Default}}<div class="suggest">Suggested: {{.Default}}</div>{{end}}
<form class="inline" method="post" action="/answer"><input type="hidden" name="run" value="{{$v.Run.ID}}"><input type="hidden" name="q" value="{{.ID}}">
<input type="text" name="answer" placeholder="Or type your own answer…"><button>Answer</button>
{{if .Default}}<button class="primary" name="accept" value="1">Use suggestion</button>{{end}}</form></div>{{end}}</div>{{end}}

{{range $v.Gates}}<div class="card attn"><div class="row"><div><span class="tag">Approval needed · {{.Role}}</span><h3>{{label .Phase}}{{if .Scope}} · {{.Scope}}{{end}}</h3></div><span class="pill st-awaiting_human">{{.Kind}} v{{.Version}}</span></div>
<p class="small mute" style="margin:4px 0 10px">{{help .Type}}</p>
{{if .Loops}}<p class="small">✔ The critic agent requested {{.Loops}} revision round(s) before this reached you.</p>{{end}}
{{if .Blocking}}<div class="card bad" style="margin:8px 0;padding:10px 14px"><b>Unresolved critic concerns</b><ul class="plain">{{range .Blocking}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
{{if .Notes}}<details><summary class="small mute">Critic notes ({{len .Notes}})</summary><ul class="plain">{{range .Notes}}<li>{{.}}</li>{{end}}</ul></details>{{end}}
{{if isMD .Kind}}<div class="doc">{{md .Content}}</div>{{else}}<pre class="doc">{{.Content}}</pre>{{end}}
<form method="post" action="/decide"><input type="hidden" name="run" value="{{$v.Run.ID}}"><input type="hidden" name="gate" value="{{.ID}}">
<textarea name="comment" placeholder="Comment (required if you request changes — it is sent back to the agent)"></textarea>
<div class="inline" style="display:flex;gap:8px;margin-top:10px;align-items:center;flex-wrap:wrap"><input type="text" name="user" value="web-user" style="max-width:170px;flex:none" title="your name for the audit trail">
<button class="ok" name="decision" value="approve">✓ Approve</button> <button class="warn" name="decision" value="request_changes">Request changes</button></div></form>
<p class="small mute">Your approval is recorded against this exact document version (hash-bound).</p></div>{{end}}
{{else if eq $v.Run.Status "active"}}
<div class="card"><b>Nothing needs you right now.</b> <span class="mute">Agents are working:</span>
{{range $v.Phases}}{{if eq .Status "in_progress"}}<div class="small" style="margin-top:6px">⏳ <b>{{label .Phase}}</b>{{if .Scope}} ({{.Scope}}){{end}} — {{step .Step}}{{if .Loops}} · revision {{.Loops}}{{end}}</div>{{end}}{{end}}</div>
{{else if eq $v.Run.Status "completed"}}<div class="card"><b>✓ Run complete.</b> <span class="mute">All phases are done and every gate was approved.</span></div>{{end}}

{{$dl := docLinks $v}}{{if $dl}}<div class="card" style="padding:12px 18px"><span class="tag">Published</span> {{range $dl}}<a href="{{.URL}}" target="_blank" rel="noopener" style="margin-left:14px">{{if eq .Kind "confluence"}}Confluence: {{.Ref}}{{else}}GitHub repo: {{.Ref}}{{end}} ↗</a>{{end}}</div>{{end}}
<h2>Pipeline</h2><div class="card"><div class="pipe">{{range $i, $p := planning $v}}<div class="node st-{{$p.Status}}" title="{{$p.Step}}"><span class="dot">{{icon $p.Status $i}}</span><span class="lbl">{{label $p.Phase}}</span><span class="stt">{{status $p.Status}}</span></div>{{end}}</div></div>
{{range stories $v}}{{$k := .Key}}<div class="card"><div class="row"><div><span class="tag">{{.Key}} · {{.Status}}</span><h3>{{.Summary}}</h3></div><span class="small mute">{{range .Reqs}}{{.}} {{end}}</span></div>
<div class="pipe mini">{{range $i, $p := phasesFor $v $k}}<div class="node st-{{$p.Status}}" title="{{$p.Step}}"><span class="dot">{{icon $p.Status $i}}</span><span class="lbl">{{label $p.Phase}}</span></div>{{end}}</div></div>{{end}}

{{$dq := doneQs $v}}{{if $dq}}<h2>Decisions &amp; assumptions</h2><div class="card">{{range $dq}}<div class="q"><span class="tag">{{label .Phase}}</span><div>{{.Text}}</div>
<div class="small"><b>{{if eq .Status "accepted_assumption"}}Assumed{{else}}Answer{{end}}:</b> {{.Answer}} <span class="mute">— {{.ResolvedBy}}</span></div></div>{{end}}</div>{{end}}

{{else if eq .Tab "documents"}}
<p class="sub" style="margin-top:14px">Every artifact is versioned and hashed. Approvals are bound to the hash shown.</p>
{{range $v.Artifacts}}<details class="card" style="padding:6px 18px"><summary>{{label .Phase}}{{if .Scope}} · {{.Scope}}{{end}} — {{.Kind}} <span class="mute small">v{{.Version}} · {{.Hash}}</span></summary>
{{if isMD .Kind}}<div class="doc">{{md .Content}}</div>{{else}}<pre class="doc">{{.Content}}</pre>{{end}}</details>{{else}}<div class="empty">Nothing yet.</div>{{end}}

{{else if eq .Tab "tickets"}}
{{range stories $v}}{{$k := .Key}}<div class="card"><div class="row"><div><span class="tag">{{with link $v "jira" .Key}}<a href="{{.}}" target="_blank" rel="noopener">{{$k}} ↗</a>{{else}}{{$k}}{{end}} · {{.Estimate}} pts · {{.Repo}}</span><h3>{{.Summary}}</h3></div><span class="pill st-{{if eq .Status "Done"}}complete{{else}}active{{end}}">{{.Status}}</span></div>
<p class="small">{{.Description}}</p><b class="small">Acceptance criteria</b><ul class="plain">{{range .Acceptance}}<li>{{.}}</li>{{end}}</ul>
<div class="small mute">Covers: {{range .Reqs}}{{.}} {{end}}</div>
{{with mrFor $v $k}}<div class="small" style="margin-top:6px">Merge request !{{.ID}} · branch <code>{{.Branch}}</code> · <b>{{.Status}}</b>{{if .MergeSHA}} · merged as <code>{{.MergeSHA}}</code>{{end}}</div>{{end}}</div>
{{else}}<div class="empty">Tickets appear after the Jira step is approved.</div>{{end}}

{{else}}
<table class="feed" style="margin-top:12px">{{range $v.Events}}<tr class="{{if or (eq .Type "agent_invoked") (eq .Type "artifact_created")}}dim{{end}}"><td>{{.At.Format "15:04:05"}}</td><td class="small mute">{{.Phase}}</td><td>{{event .Type .Detail}}</td><td class="small mute">{{.Actor}}</td></tr>{{end}}</table>
{{end}}
</main>{{template "foot" .}}{{end}}`
