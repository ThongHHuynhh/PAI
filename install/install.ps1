# Installs the pai CLI for the user who runs it, on their PATH, unless this machine has one; with
# an api key it logs that CLI in. With a server URL and device code it installs the CLI for the machine instead and
# makes this Windows machine a worker for that user, after a UAC prompt. With server it also makes this
# machine the PAI server, from the provider keys set in this shell, after a UAC prompt, and logs the CLI
# in to it; server quickstart then opens the quickstart in the TUI, and -Url <url> reaches the server at url,
# which your own proxy carries to it, with no tunnel. The release stamps its version.
#   & ([scriptblock]::Create((irm https://github.com/@REPO@/releases/download/@VERSION@/install.ps1))) [api key | <server url> <device code> | server [quickstart] [-Url <url>]]
param([Parameter(Position = 0)][string]$Token, [Parameter(Position = 1)][string]$Code, [string]$Url)
$ErrorActionPreference = 'Stop'
$quick = $Token -eq 'server' -and $Code -eq 'quickstart'
$withUrl = $PSBoundParameters.ContainsKey('Url')
if (($Token -and !$Code -and $Token -ne 'server' -and $Token -notlike 'pai_key_*') -or ($withUrl -and ($Token -ne 'server' -or ($Code -and !$quick)))) {
  throw 'usage: install.ps1 [api key | <server url> <device code> | server [quickstart] [-Url <url>]]'
}
$ProgressPreference = 'SilentlyContinue'
$dl = 'https://github.com/@REPO@/releases/download/@VERSION@'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$d = New-Item -ItemType Directory (Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid()))
# Download saves $url as $out, up to three tries, since one bad read would fail the whole install.
function Download($url, $out) {
  foreach ($n in 1..3) {
    try { Invoke-WebRequest $url -OutFile $out -UseBasicParsing; return } catch { if ($n -eq 3) { throw } }
    Start-Sleep $n
  }
}
Download "$dl/SHA256SUMS" (Join-Path $d SHA256SUMS)
$sums = Get-Content (Join-Path $d SHA256SUMS)
# Fetch gets this machine's build of release binary $name into $d as $name.exe, checked against
# SHA256SUMS. pai-server install finds pai.exe there.
function Fetch($name) {
  $f = "$name-windows-$arch.exe"
  $t = Join-Path $d "$name.exe"
  Write-Host "downloading $name @VERSION@"
  Download "$dl/$f" $t
  $want = ($sums | Where-Object { $_ -match " $([regex]::Escape($f))$" }) -split ' ' | Select-Object -First 1
  if ($want -ne (Get-FileHash $t).Hash.ToLower()) { throw "$f does not match SHA256SUMS" }
  $t
}
if ($Token -eq 'server' -and (!$Code -or $quick)) {
  # install places pai-server in Program Files, runs it as a service, makes this machine the
  # worker admin with the pai beside it, which places that in Program Files, and logs that in
  Fetch pai | Out-Null
  & (Fetch pai-server) install @(if ($quick) { 'quickstart' }) @(if ($withUrl) { '--url', $Url })
  if ($LASTEXITCODE) { exit $LASTEXITCODE }
  $bin = "$env:ProgramFiles\pai"
} elseif ($Code) {
  & (Fetch pai) install $Token $Code
  if ($LASTEXITCODE) { exit $LASTEXITCODE }
  $bin = "$env:ProgramFiles\pai"
} else {
  # A CLI this machine has is used: a worker's or server's in Program Files, else this user's.
  $bin = "$env:ProgramFiles\pai", "$env:LOCALAPPDATA\Programs\pai" | Where-Object { Test-Path "$_\pai.exe" } | Select-Object -First 1
  if ($bin) {
    if (!$Token) { Write-Output "pai is installed: $bin\pai.exe" }
  } else {
    $bin = "$env:LOCALAPPDATA\Programs\pai"
    New-Item -ItemType Directory -Force $bin | Out-Null
    Move-Item -Force (Fetch pai) "$bin\pai.exe"
    # SetEnvironmentVariable tells running programs, so a shell Explorer starts next finds pai.
    $p = [string][Environment]::GetEnvironmentVariable('Path', 'User')
    if (($p -split ';') -notcontains $bin) {
      [Environment]::SetEnvironmentVariable('Path', "$($p.TrimEnd(';'));$bin".TrimStart(';'), 'User')
    }
    Write-Output 'pai @VERSION@ installed'
  }
}
# $bin is on the machine's or the user's PATH now, which this shell read before
if (($env:Path -split ';') -notcontains $bin) { $env:Path += ";$bin" }
Remove-Item -Recurse $d
if ($Token -like 'pai_key_*') {
  & "$bin\pai.exe" login $Token
  if ($LASTEXITCODE) { exit $LASTEXITCODE }
}
