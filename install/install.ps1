# Installs the pai CLI for the user who runs it, on their PATH, unless this machine has one; with
# an api key it logs that CLI in. With a server URL and device code it installs the CLI for the machine instead and
# makes this Windows machine a worker for that user, after a UAC prompt. With server it also makes this
# machine the PAI server, from the provider keys set in this shell, after a UAC prompt, and logs the CLI
# in to it; server quickstart then opens the quickstart in the TUI, and -Url <url> reaches the server at url,
# which your own proxy carries to it, with no tunnel. The release stamps its version.
#   & ([scriptblock]::Create((irm @DL@/install.ps1))) [api key | <server url> <device code> | server [quickstart] [-Url <url>]]
param([Parameter(Position = 0)][string]$Token, [Parameter(Position = 1)][string]$Code, [string]$Url)
$ErrorActionPreference = 'Stop'
$quick = $Token -eq 'server' -and $Code -eq 'quickstart'
$withUrl = $PSBoundParameters.ContainsKey('Url')
if (($Token -and !$Code -and $Token -ne 'server' -and $Token -notlike 'pai_key_*') -or ($withUrl -and ($Token -ne 'server' -or ($Code -and !$quick)))) {
  throw 'usage: install.ps1 [api key | <server url> <device code> | server [quickstart] [-Url <url>]]'
}
$ProgressPreference = 'SilentlyContinue'
$dl = '@DL@'
$key = '@KEY@'
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
# NoSig goes on without the signature, the checksums still guarding each file, unless
# PAI_REQUIRE_SIGNATURE=1 asks for it.
function NoSig($why) {
  if ($env:PAI_REQUIRE_SIGNATURE -eq '1') { throw "cannot check the release signature: $why" }
  Write-Warning "release signature not checked ($why); checksums still are"
}
# SHA256SUMS counts once the release key signed it. Windows has no ed25519 of its own: OpenSSL 3
# checks it, on PATH or the one Git for Windows ships. The key is its 32 raw bytes in base64,
# behind the fixed DER header of an ed25519 public key.
$ossl = (Get-Command openssl.exe -ErrorAction SilentlyContinue).Source
if (!$ossl) {
  # Git for Windows, for every user or for this one, wherever git is found
  $git = (Get-Command git.exe -ErrorAction SilentlyContinue).Source
  $roots = @("$env:ProgramFiles\Git", "$env:LOCALAPPDATA\Programs\Git") + @(if ($git) { Split-Path (Split-Path $git) })
  $ossl = foreach ($r in $roots) { foreach ($s in 'usr\bin', 'mingw64\bin', 'clangarm64\bin') { Join-Path $r "$s\openssl.exe" } }
  $ossl = $ossl | Where-Object { Test-Path $_ } | Select-Object -First 1
}
if (!$key -or $key.StartsWith('@')) {
  NoSig 'this installer carries no release key'
} elseif (!$ossl -or !((& $ossl version) -match '^OpenSSL [3-9]')) {
  NoSig 'it needs OpenSSL 3, which Git for Windows includes'
} else {
  Download "$dl/SHA256SUMS.sig" (Join-Path $d SHA256SUMS.sig)
  [IO.File]::WriteAllBytes((Join-Path $d key.der), [byte[]](0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00) + [Convert]::FromBase64String($key))
  # a native command's stderr must not stop the script: its exit code says
  $ErrorActionPreference = 'Continue'
  # a call that cannot start leaves $LASTEXITCODE as it was: start each from a failure
  $global:LASTEXITCODE = 1
  & $ossl pkey -pubin -inform DER -in (Join-Path $d key.der) -out (Join-Path $d key.pem) | Out-Null
  $loaded = $? -and $LASTEXITCODE -eq 0
  $ok = $false
  if ($loaded) {
    $global:LASTEXITCODE = 1
    & $ossl pkeyutl -verify -pubin -inkey (Join-Path $d key.pem) -rawin -in (Join-Path $d SHA256SUMS) -sigfile (Join-Path $d SHA256SUMS.sig) | Out-Null
    $ok = $? -and $LASTEXITCODE -eq 0
  }
  $ErrorActionPreference = 'Stop'
  # an OpenSSL that cannot load an ed25519 key cannot check; only a failed check is tampering
  if (!$loaded) { NoSig 'this OpenSSL cannot load an ed25519 key' }
  elseif (!$ok) { throw "release @VERSION@: SHA256SUMS is not signed by the release key" }
  else { Write-Host 'release @VERSION@: signature checked' }
}
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
