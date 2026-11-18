# Copies a private GitHub repo's PAI releases into Dir, laid out the way a
# build stamped with release.Base expects: Dir\<version>\<file>, and Dir\latest\
# holding the newest stable release. Run it on a schedule (docs/download-server.md).
#
#   $env:GH_TOKEN = '<read-only token>'; .\sync.ps1 -Repo owner/repo -Dir D:\pai-releases
#
# A release is published only once every file matches its SHA256SUMS; a broken
# download is left in Dir\.tmp-<version> and retried on the next run.
param([Parameter(Mandatory)][string]$Repo, [Parameter(Mandatory)][string]$Dir)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force $Dir | Out-Null
$lock = [IO.File]::Open((Join-Path $Dir '.sync.lock'), 'OpenOrCreate', 'ReadWrite', 'None')
try {
  $json = gh release list -R $Repo --limit 100 --json tagName,isDraft,isPrerelease
  if ($LASTEXITCODE -ne 0) { throw 'gh release list failed' }
  # Windows PowerShell 5.1 passes a JSON array down the pipeline as one item:
  # the parentheses make it one item per release.
  $releases = @(($json | Out-String | ConvertFrom-Json)) | Where-Object { -not $_.isDraft }

  # One release that fails is reported and skipped, so it never holds back the rest.
  $failed = $false
  foreach ($r in $releases) {
    $tag = $r.tagName
    if ($tag -notmatch '^v[0-9][A-Za-z0-9.-]*$') { Write-Warning "skip ${tag}: not a version tag"; continue }
    $dest = Join-Path $Dir $tag
    if (Test-Path (Join-Path $dest 'SHA256SUMS')) { continue }
    $tmp = Join-Path $Dir ".tmp-$tag"
    try {
      if (Test-Path $tmp) { Remove-Item -Recurse -Force $tmp }
      gh release download $tag -R $Repo -D $tmp
      if ($LASTEXITCODE -ne 0) { throw 'download failed' }
      foreach ($line in Get-Content (Join-Path $tmp 'SHA256SUMS')) {
        $want, $file = $line -split '\s+', 2
        if (-not $file) { continue }
        $got = (Get-FileHash (Join-Path $tmp $file.TrimStart('*')) -Algorithm SHA256).Hash.ToLower()
        if ($got -ne $want) { throw "$file does not match SHA256SUMS" }
      }
      Rename-Item $tmp $tag
      Write-Output "added $tag"
    } catch {
      Write-Warning "FAILED ${tag}: $_ (left in $tmp)"
      $failed = $true
    }
  }

  # latest\ is the newest stable release, as GitHub's releases/latest is.
  $stable = $releases | Where-Object { -not $_.isPrerelease } | Select-Object -First 1
  if ($stable) {
    $src = Join-Path $Dir $stable.tagName
    $cur = Join-Path $Dir 'latest\VERSION'
    $same = (Test-Path $cur) -and ((Get-Content $cur -Raw) -eq (Get-Content (Join-Path $src 'VERSION') -Raw))
    if ((Test-Path $src) -and -not $same) {
      $new = Join-Path $Dir '.latest-new'; $old = Join-Path $Dir '.latest-old'; $latest = Join-Path $Dir 'latest'
      foreach ($p in $new, $old) { if (Test-Path $p) { Remove-Item -Recurse -Force $p } }
      Copy-Item -Recurse $src $new
      if (Test-Path $latest) { Rename-Item $latest '.latest-old' }
      Rename-Item $new 'latest'
      if (Test-Path $old) { Remove-Item -Recurse -Force $old }
      Write-Output "latest is $($stable.tagName)"
    }
  }
} finally { $lock.Close() }
if ($failed) { exit 1 }
