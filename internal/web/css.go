package web

// Dark theme is the default; a toggle in the header switches to light (remembered in localStorage).
const baseCSS = `{{define "head"}}<!doctype html><html lang="en" data-theme="dark"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · SDLC Orchestrator</title>
<script>{const q=new URLSearchParams(location.search).get('theme');if(q)localStorage.setItem('theme',q);document.documentElement.dataset.theme=localStorage.getItem('theme')||'dark'}</script>
<style>
:root,[data-theme=dark]{--bg:#0a0f1e;--bg2:#0d1428;--card:#121a30;--card2:#18223d;--ink:#eaeefb;--mute:#a3afcc;--line:#27324f;--brand:#8b9bff;--brand2:#b48bff;--on-brand:#0a0f1e;
--done:#3ddc97;--wait:#ffbf47;--work:#5ab0ff;--bad:#ff6b7a;--idle:#6b7799;--soft:#1a2440;--shadow:0 6px 24px rgba(0,0,0,.35);--glow:rgba(139,155,255,.16)}
[data-theme=light]{--bg:#f3f5fb;--bg2:#e9edf8;--card:#ffffff;--card2:#f7f8fd;--ink:#141a2e;--mute:#59647f;--line:#dde2ef;--brand:#4f46e5;--brand2:#7c3aed;--on-brand:#fff;
--done:#12935f;--wait:#b86e00;--work:#1d64d8;--bad:#d1293d;--idle:#8c97b0;--soft:#eef1f9;--shadow:0 4px 18px rgba(30,40,90,.08);--glow:rgba(79,70,229,.12)}
*{box-sizing:border-box}html{color-scheme:dark light}[data-theme=light]{color-scheme:light}[data-theme=dark]{color-scheme:dark}
body{margin:0;background:radial-gradient(1200px 500px at 70% -10%,var(--glow),transparent),var(--bg);background-attachment:fixed;color:var(--ink);font:15px/1.6 Inter,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
a{color:var(--brand);text-decoration:none}a:hover{text-decoration:underline}
header.top{position:sticky;top:0;z-index:5;backdrop-filter:blur(10px);background:color-mix(in srgb,var(--bg2) 82%,transparent);border-bottom:1px solid var(--line);padding:11px 28px;display:flex;align-items:center;gap:12px}
header.top b{font-size:16px;letter-spacing:.01em}.logo{width:30px;height:30px;border-radius:9px;background:linear-gradient(135deg,var(--brand),var(--brand2));color:var(--on-brand);display:grid;place-items:center;font-weight:800;font-size:15px}
.spacer{flex:1}.theme{padding:6px 12px;border-radius:99px;font-size:13px;background:var(--card);border:1px solid var(--line);color:var(--ink);cursor:pointer}
main{max-width:1060px;margin:0 auto;padding:28px 22px 90px}
h1{font-size:28px;margin:0;letter-spacing:-.01em}h2{font-size:13px;margin:34px 0 12px;text-transform:uppercase;letter-spacing:.09em;color:var(--mute);font-weight:700}h3{font-size:16.5px;margin:2px 0 0}
.sub{color:var(--mute);margin:6px 0 0}.mute{color:var(--mute)}.small{font-size:13.5px}
.card{background:linear-gradient(180deg,var(--card),var(--card2));border:1px solid var(--line);border-radius:16px;padding:20px 22px;margin:14px 0;box-shadow:var(--shadow)}
.card.attn{border-color:color-mix(in srgb,var(--wait) 65%,var(--line));box-shadow:0 0 0 4px color-mix(in srgb,var(--wait) 12%,transparent),var(--shadow)}
.card.bad{border-color:color-mix(in srgb,var(--bad) 70%,var(--line))}
a.cardlink{color:inherit;text-decoration:none;display:block}a.cardlink .card{transition:transform .12s,border-color .12s}a.cardlink:hover .card{transform:translateY(-2px);border-color:var(--brand)}
.titlebar{display:flex;align-items:center;gap:12px;flex-wrap:wrap;margin-top:6px}
.pill{display:inline-flex;align-items:center;gap:6px;padding:3px 12px;border-radius:99px;font-size:12.5px;font-weight:700;background:var(--soft);color:var(--mute);border:1px solid var(--line)}
.pill.st-completed,.pill.st-complete{background:color-mix(in srgb,var(--done) 15%,transparent);color:var(--done);border-color:color-mix(in srgb,var(--done) 40%,transparent)}
.pill.st-active,.pill.st-in_progress{background:color-mix(in srgb,var(--work) 15%,transparent);color:var(--work);border-color:color-mix(in srgb,var(--work) 40%,transparent)}
.pill.st-awaiting_human{background:color-mix(in srgb,var(--wait) 16%,transparent);color:var(--wait);border-color:color-mix(in srgb,var(--wait) 45%,transparent)}
.pill.st-blocked,.pill.st-failed,.pill.st-cancelled{background:color-mix(in srgb,var(--bad) 15%,transparent);color:var(--bad);border-color:color-mix(in srgb,var(--bad) 45%,transparent)}
.pill.st-active::before{content:"";width:7px;height:7px;border-radius:50%;background:var(--work);animation:pulse 1.6s infinite}
.tabs{display:flex;gap:6px;margin:22px 0 6px;padding:5px;background:var(--card);border:1px solid var(--line);border-radius:12px;width:fit-content;max-width:100%;overflow-x:auto}
.tabs a{padding:8px 16px;color:var(--mute);border-radius:8px;font-weight:600;white-space:nowrap;text-decoration:none}.tabs a:hover{color:var(--ink)}
.tabs a.on{background:linear-gradient(135deg,var(--brand),var(--brand2));color:var(--on-brand)}.tabs .badge{background:var(--wait);color:#1a1200;border-radius:99px;font-size:11px;padding:1px 8px;margin-left:7px;font-weight:800}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin:18px 0 4px}
.tile{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:14px 16px}.tile b{display:block;font-size:24px;line-height:1.2}.tile span{color:var(--mute);font-size:13px}
.bar{height:9px;background:var(--soft);border-radius:99px;overflow:hidden;border:1px solid var(--line)}.bar i{display:block;height:100%;background:linear-gradient(90deg,var(--done),var(--work));border-radius:99px}
.pipe{display:flex;margin:6px 0 2px;overflow-x:auto;padding:4px 0 8px}
.node{flex:1;min-width:112px;text-align:center;position:relative}
.node:not(:last-child)::after{content:"";position:absolute;top:21px;left:calc(50% + 24px);right:calc(-50% + 24px);height:3px;border-radius:3px;background:var(--line)}
.node.st-complete:not(:last-child)::after{background:var(--done)}
.dot{width:44px;height:44px;border-radius:50%;display:inline-grid;place-items:center;font-weight:800;font-size:16px;background:var(--soft);color:var(--idle);border:2px solid var(--line)}
.node.st-complete .dot{background:var(--done);border-color:var(--done);color:#07241a}
.node.st-in_progress .dot{border-color:var(--work);color:var(--work);background:color-mix(in srgb,var(--work) 12%,var(--soft));animation:pulse 1.6s infinite}
.node.st-awaiting_human .dot{background:var(--wait);border-color:var(--wait);color:#241800}
.node.st-blocked .dot{background:var(--bad);border-color:var(--bad);color:#2a0007}.node.st-skipped .dot{opacity:.45}
.node .lbl{display:block;font-size:14px;font-weight:700;margin-top:9px}.node .stt{display:block;font-size:12.5px;color:var(--mute)}
.node.st-in_progress .stt{color:var(--work)}.node.st-awaiting_human .stt{color:var(--wait)}.node.st-complete .stt{color:var(--done)}
@keyframes pulse{0%{box-shadow:0 0 0 0 color-mix(in srgb,var(--work) 45%,transparent)}70%{box-shadow:0 0 0 10px transparent}100%{box-shadow:0 0 0 0 transparent}}
.pipe.mini .node{min-width:84px}.pipe.mini .dot{width:30px;height:30px;font-size:13px}.pipe.mini .node:not(:last-child)::after{top:14px;left:calc(50% + 18px);right:calc(-50% + 18px);height:2px}.pipe.mini .lbl{font-size:12.5px;margin-top:6px}
.doc{background:var(--bg2);border:1px solid var(--line);border-radius:12px;padding:8px 26px 18px;max-height:520px;overflow:auto;font-size:15.5px;line-height:1.75}
.doc h2,.doc h3,.doc h4,.doc h5{margin:22px 0 6px;text-transform:none;letter-spacing:0;color:var(--ink)}.doc h2{font-size:21px;border-bottom:1px solid var(--line);padding-bottom:6px}.doc h3{font-size:17.5px}.doc h4{font-size:16px}
.doc p{margin:8px 0}.doc ul,.doc ol{margin:8px 0;padding-left:24px}.doc li{margin:4px 0}.doc strong{color:var(--ink)}
.doc code{background:var(--soft);border:1px solid var(--line);padding:1px 6px;border-radius:5px;font-size:13.5px;color:var(--brand)}.doc pre{background:var(--card);border:1px solid var(--line);padding:12px;border-radius:10px;overflow:auto;font-size:13.5px}
pre.doc{white-space:pre-wrap;font:13.5px/1.6 ui-monospace,"Cascadia Code",Consolas,monospace;padding:16px}
.row{display:flex;gap:12px;align-items:flex-start;justify-content:space-between;flex-wrap:wrap}
form.inline{display:flex;gap:8px;flex-wrap:wrap;margin-top:10px;align-items:flex-start}
input[type=text],textarea,select{font:inherit;color:var(--ink);background:var(--bg2);border:1px solid var(--line);border-radius:10px;padding:10px 12px;outline:none}
input[type=text]:focus,textarea:focus,select:focus{border-color:var(--brand);box-shadow:0 0 0 3px var(--glow)}
textarea{width:100%;min-height:76px;resize:vertical;margin-top:12px}input[type=text]{min-width:200px;flex:1}::placeholder{color:var(--idle)}
button{font:inherit;font-weight:700;border-radius:10px;border:1px solid var(--line);background:var(--card2);color:var(--ink);padding:9px 18px;cursor:pointer;transition:transform .08s,filter .12s}
button:hover{filter:brightness(1.15);transform:translateY(-1px)}button:active{transform:none}
button.primary{background:linear-gradient(135deg,var(--brand),var(--brand2));border-color:transparent;color:var(--on-brand)}
button.ok{background:var(--done);border-color:var(--done);color:#06241a}button.warn{background:transparent;border-color:var(--wait);color:var(--wait)}
.q{border-top:1px solid var(--line);padding:16px 0}.q:first-of-type{border-top:0}
.tag{font-size:11.5px;font-weight:800;text-transform:uppercase;letter-spacing:.07em;color:var(--brand)}
.suggest{background:var(--soft);border-left:3px solid var(--brand);border-radius:8px;padding:9px 14px;margin:9px 0;font-size:14.5px}
ul.plain{margin:6px 0;padding-left:22px}table{border-collapse:collapse;width:100%}td,th{padding:10px 12px;border-bottom:1px solid var(--line);text-align:left;vertical-align:top;font-size:14px}
details{margin:8px 0}summary{cursor:pointer;font-weight:700;padding:8px 0;color:var(--ink)}summary:hover{color:var(--brand)}
.feed{background:var(--card);border:1px solid var(--line);border-radius:14px;overflow:hidden}.feed td:first-child{white-space:nowrap;color:var(--mute);width:84px;font-variant-numeric:tabular-nums}.feed tr.dim td{color:var(--mute);font-size:13px}.feed tr:last-child td{border-bottom:0}
.empty{text-align:center;padding:34px;color:var(--mute)}
::-webkit-scrollbar{width:10px;height:10px}::-webkit-scrollbar-thumb{background:var(--line);border-radius:8px}
</style></head><body><header class="top"><div class="logo">S</div><b><a href="/" style="color:inherit;text-decoration:none">SDLC Orchestrator</a></b><span class="mute small">idea → PRD → design → tickets → code → review</span><span class="spacer"></span>
<button class="theme" type="button" onclick="var t=document.documentElement.dataset.theme==='dark'?'light':'dark';document.documentElement.dataset.theme=t;localStorage.setItem('theme',t)">◐ Theme</button></header>{{end}}
{{define "foot"}}<script>
setInterval(async()=>{
 const a=document.activeElement; if(a&&/INPUT|TEXTAREA/.test(a.tagName))return;
 for(const f of document.querySelectorAll('textarea,input[type=text]')){ if(f.defaultValue!==f.value)return }
 for(const d of document.querySelectorAll('.doc')){ if(d.scrollTop>0)return }
 if(document.querySelector('details[open]'))return;
 try{const r=await fetch(location.href);const t=await r.text();const n=new DOMParser().parseFromString(t,'text/html').querySelector('main');if(n)document.querySelector('main').replaceWith(n)}catch(e){}
},4000);
</script></body></html>{{end}}`
