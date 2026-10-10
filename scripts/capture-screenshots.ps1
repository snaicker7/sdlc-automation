# Drives a no-auto-approve orchestrator through a full run using the dashboard's own forms and
# captures screenshots of each screen with headless Edge. Run it against `sdlc up -agent mock`
# (see README "Screenshots"). Requires PowerShell 7 and Microsoft Edge.
param(
  [string]$Base = 'http://localhost:8090',
  [string]$Out = 'docs\screenshots',
  [string]$Bin = '.\sdlc.exe',
  [string]$Edge = 'C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe',
  [int]$TimeoutMinutes = 25,
  [string]$RunId = ''
)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force $Out | Out-Null
$Out = (Resolve-Path $Out).Path

function Shot($name, $path, $h = 1100, $w = 1200) {
  & $Edge --headless=new --disable-gpu --hide-scrollbars "--window-size=$w,$h" "--screenshot=$Out\$name.png" "$Base$path" 2>&1 | Out-Null
  Write-Host ("shot {0}" -f $name)
}
function Api($id) { Invoke-RestMethod "$Base/api/run/$id" }
function Post($path, $form) {
  Invoke-WebRequest "$Base$path" -Method Post -Body $form -UseBasicParsing -SkipHttpErrorCheck
}

# who approves what (shows up in the audit trail)
$people = @{ product_manager = 'priya-pm'; tech_lead = 'arjun-lead'; developer = 'sam-dev'; reviewer = 'mei-reviewer' }
# first gate of each kind that deserves a screenshot
$gateShots = @{ pm_approval = '04-approval-prd'; jira_preview_approval = '05-approval-jira-preview'; plan_approval = '06-approval-plan'; reviewer_approval = '07-approval-review' }

if ($RunId) {
  $id = $RunId # reuse an already-started run
} else {
  Shot '02-new-run-form' '/?new=1' 760
  $idea = 'A simple document organizer: a small Go command-line/HTTP tool where a user adds text/markdown documents with a title and tags, lists them, and searches them by tag or text. Local single-user app, data persisted to a JSON file, standard library only.'
  $r = Post '/start' @{ title = 'Document organizer'; idea = $idea; size = 'standard'; user = 'priya-pm' }
  # the POST redirects to /run/<id>; the final URL carries the id
  $id = $r.BaseResponse.RequestMessage.RequestUri.AbsolutePath.Split('/')[-1]
}
if (-not $id) { throw 'could not determine run id' }
Write-Host "run $id"

$seen = @{}
$deadline = (Get-Date).AddMinutes($TimeoutMinutes)
while ((Get-Date) -lt $deadline) {
  $v = Api $id
  if ($v.Run.Status -ne 'active') { break }
  foreach ($p in @($v.Phases)) {
    if ($p.Status -eq 'blocked') { throw "phase $($p.Phase) $($p.Scope) blocked: $($p.LastError)" }
  }

  $open = @($v.Questions | Where-Object { $_.Status -eq 'open' })
  if ($open.Count -gt 0) {
    if (-not $seen['q']) { Shot '03-questions' "/run/$id" 1300; $seen['q'] = 1 }
    foreach ($q in $open) {
      if ($q.Phase -eq 'add') { # answer one in the human's own words, accept the rest
        Post '/answer' @{ run = $id; q = $q.ID; answer = 'Persist to a single JSON file in the user data dir, written atomically (temp file + rename).'; user = 'arjun-lead' } | Out-Null
      } else {
        Post '/answer' @{ run = $id; q = $q.ID; accept = '1'; user = 'priya-pm' } | Out-Null
      }
    }
    continue
  }

  if ($v.Gates -and @($v.Gates).Count -gt 0) {
    $g = @($v.Gates)[0]
    if ($gateShots.ContainsKey($g.Type) -and -not $seen[$g.Type]) { Shot $gateShots[$g.Type] "/run/$id" 1600; $seen[$g.Type] = 1 }
    Post '/decide' @{ run = $id; gate = $g.ID; decision = 'approve'; user = $people[$g.Role]; comment = '' } | Out-Null
    continue
  }

  $waiting = @($v.Phases | Where-Object { $_.Status -eq 'awaiting_human' -and $_.Step -eq 'publish' })
  if ($waiting.Count -gt 0 -and -not $seen['pr']) { Shot '08-waiting-pr-merge' "/run/$id" 1250; $seen['pr'] = 1 }
  Start-Sleep -Seconds 2
}

$v = Api $id
if ($v.Run.Status -ne 'completed') { throw "run did not complete (status $($v.Run.Status))" }
Start-Sleep -Seconds 2
Shot '01-runs-list' '/' 560
Shot '09-overview-complete' "/run/$id" 1500
Shot '10-documents' "/run/${id}?tab=documents&open=1" 1900
Shot '11-tickets' "/run/${id}?tab=tickets" 1500
Shot '12-activity' "/run/${id}?tab=activity" 1700
Shot '13-light-theme' "/run/${id}?theme=light" 1500
Shot '14-dark-restore' "/?theme=dark" 300 | Out-Null
Remove-Item "$Out\14-dark-restore.png" -ErrorAction SilentlyContinue

$cli = Join-Path (Split-Path $Out -Parent) 'cli-output.txt'
@("`$ sdlc status $id", (& $Bin status $id 2>&1 | Out-String), "`$ sdlc trace $id", (& $Bin trace $id 2>&1 | Out-String), "`$ sdlc ci-check $id", (& $Bin ci-check $id 2>&1 | Out-String)) | Set-Content $cli -Encoding utf8
Write-Host "done: run $id"
