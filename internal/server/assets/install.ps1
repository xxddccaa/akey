# akey installer for Windows PowerShell 5.1+ (rendered by `akey serve`).
# Extra pull flags: & ([scriptblock]::Create((New-Object Net.WebClient).DownloadString('<server>/i.ps1'))) --only codex
# Runs inside a script block so nothing leaks into the caller's session, and
# never calls `exit` (that would close the window when run via iex).
& {
    $ErrorActionPreference = 'Stop'
    $Server = '{{psq .Server}}'
    $Pin = '{{psq .Pin}}'
    $CertSHA256 = '{{psq .CertSHA256}}'

    $dir = Join-Path $env:LOCALAPPDATA 'akey'
    $exe = Join-Path $dir 'akey.exe'
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072
        if ($CertSHA256) {
            # Accept only the server certificate whose DER SHA-256 matches.
            [Net.ServicePointManager]::ServerCertificateValidationCallback = {
                param($s, $c, $ch, $e)
                if ($null -eq $c) { return $false }
                $h = [BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($c.GetRawCertData())).Replace('-', '')
                return ($h -eq '{{psq .CertSHA256}}')
            }
        }

        $arch = $env:PROCESSOR_ARCHITEW6432
        if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
        switch ($arch) {
            'AMD64' { $goarch = 'amd64' }
            'ARM64' { $goarch = 'arm64' }
            default { throw "akey: unsupported architecture: $arch" }
        }
        $name = "akey-windows-$goarch.exe"

        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $tmp = Join-Path $dir "$name.download"
        $wc = New-Object Net.WebClient
        Write-Host "akey: downloading $name from $Server"
        $wc.DownloadFile("$Server/dl/$name", $tmp)
        $sums = $wc.DownloadString("$Server/dl/SHA256SUMS")

        $want = $null
        foreach ($line in ($sums -split "`r?`n")) {
            $f = $line.Trim() -split '\s+'
            if ($f.Count -ge 2 -and $f[1].TrimStart('*') -eq $name) { $want = $f[0]; break }
        }
        if (-not $want) { Remove-Item -Force -LiteralPath $tmp; throw "akey: no checksum for $name in SHA256SUMS" }
        $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $tmp).Hash
        if ($got -ne $want) { Remove-Item -Force -LiteralPath $tmp; throw "akey: checksum mismatch for $name (got $got, want $want)" }

        Move-Item -Force -LiteralPath $tmp -Destination $exe
        Write-Host "akey: installed $exe"

        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $parts = @()
        if ($userPath) { $parts = @($userPath -split ';' | Where-Object { $_ }) }
        if ($parts -notcontains $dir) {
            [Environment]::SetEnvironmentVariable('Path', (($parts + $dir) -join ';'), 'User')
            Write-Host "akey: added $dir to your user PATH (open a new terminal to use 'akey')"
        }
        if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }
    } finally {
        # Do not leave the pinned callback installed in this session.
        [Net.ServicePointManager]::ServerCertificateValidationCallback = $null
    }

    # Windows PowerShell drops empty native arguments, so only pass --pin when set.
    $pullArgs = @('pull', '--server', $Server)
    if ($Pin) { $pullArgs += @('--pin', $Pin) }
    $pullArgs += $args
    & $exe @pullArgs
    if ($LASTEXITCODE -ne 0) { Write-Warning "akey pull exited with code $LASTEXITCODE" }
} @args
