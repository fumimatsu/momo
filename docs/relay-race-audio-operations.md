# Relay レース音声の適用手順

## 対象範囲

Relay が Race Control の `race_state v2` から LAP 完了と GOAL を検出し、内部 TTS service で生成した
Opus 音声を Pilot の既存 WebRTC PeerConnection へ追加配信する。

固定 radio cue は Pilot の Web asset で再生する。生成 TTS は Relay の audio track で再生するため、
Relay binary、Viewer web assets、TTS service の 3 つがそろわないと完全には動作しない。

| 接続先 | 生成 TTS | 固定 radio cue | 備考 |
| --- | --- | --- | --- |
| Relay Pilot / LAN | 対応 | 対応 | Relay との PeerConnection で受信する |
| Relay Pilot / Ayame | 対応 | 対応 | Ayame は signaling のみで、音声は Relay から受信する |
| Direct Viewer | 非対応 | 非対応 | Relay 専用契約へ依存しない |
| Web Observer | 非対応 | 非対応 | 現行実装は Pilot role に限定する |

Web Observer は 4 台分の PeerConnection を同時に持つ。各車両の track へ同じ仕組みを追加すると音声が
重複し、現在の車両単位 detector では全車向け実況にもならない。Observer へ対応する場合は、Relay server
単位でレース全体を監視する commentary source と、Observer が 1 本だけ購読する audio endpoint を別途実装する。

## 配置

現在の試験構成:

| role | host | endpoint |
| --- | --- | --- |
| Relay / Race Control | `192.168.11.100` | Relay `:8090` |
| TTS service | `192.168.11.105` | `:18090` |

TTS service の IP は起動前に確認する。DHCP で変わった場合は Relay の
`MOMO_RACE_AUDIO_SERVICE_URL` も更新する。

## TTS service の起動

token は Git 管理外の次のファイルへ保存する。

```text
%LOCALAPPDATA%\MomoFPV\secrets\race-audio-service-token.txt
```

初回だけ 32 bytes の token を生成する。値は console や log へ出力しない。

```powershell
$secretDirectory = Join-Path $env:LOCALAPPDATA 'MomoFPV\secrets'
$tokenFile = Join-Path $secretDirectory 'race-audio-service-token.txt'
New-Item -ItemType Directory -Force -Path $secretDirectory | Out-Null
$token = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
[IO.File]::WriteAllText($tokenFile, $token)
```

file ACL は現在の Windows user と SYSTEM だけに制限する。起動時は token を process environment へ渡す。

```powershell
cd C:\src\momo\tools\race-audio-service
$tokenFile = Join-Path $env:LOCALAPPDATA 'MomoFPV\secrets\race-audio-service-token.txt'
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = [IO.File]::ReadAllText($tokenFile).Trim()
uv run python .\race_audio_service.py `
  --listen 0.0.0.0:18090 `
  --engine kokoro
```

起動後、同じ PC で確認する。

```powershell
Invoke-RestMethod http://127.0.0.1:18090/healthz
```

次に `11.100` から確認する。

```powershell
Invoke-RestMethod http://192.168.11.105:18090/healthz
```

到達できない場合は、IP と Windows Firewall の `18090/tcp` inbound rule を確認する。Firewall rule は
送信元 `192.168.11.100` に限定する。

管理者 PowerShell で初回だけ実行する。

```powershell
New-NetFirewallRule `
  -DisplayName 'Momo Race Audio Service 18090 from Relay' `
  -Direction Inbound `
  -Action Allow `
  -Protocol TCP `
  -LocalPort 18090 `
  -RemoteAddress 192.168.11.100 `
  -Profile Any
```

## `11.100` で TTS service と Relay を同居させる

常用構成では TTS service を `11.100` へ置く。loopback の `127.0.0.1:18090` だけに bind すれば、
LAN 向け firewall rule、TTS host の固定 IP、別 PC の稼働維持が不要になる。

### 初回セットアップ

`11.100` の更新済み `momo` repository で実行する。model は Git に含まれないため、repository の pull
だけでは起動できない。

```powershell
cd C:\src\momo\tools\race-audio-service
uv sync --group dev
.\download-kokoro-models.ps1
```

token は Git 管理外へ 1 回だけ生成する。TTS service と Relay は同じ token file を読む。

```powershell
$secretDirectory = Join-Path $env:LOCALAPPDATA 'MomoFPV\secrets'
$tokenFile = Join-Path $secretDirectory 'race-audio-service-token.txt'
New-Item -ItemType Directory -Force -Path $secretDirectory | Out-Null
if (-not (Test-Path -LiteralPath $tokenFile)) {
  $token = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
  [IO.File]::WriteAllText($tokenFile, $token)
}
```

token file は Relay を起動する Windows user と SYSTEM だけが読める ACL にする。token 値を command line、
log、repository へ書かない。

```powershell
$user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
& icacls.exe $secretDirectory `
  '/inheritance:r' `
  '/grant:r' `
  "${user}:(OI)(CI)F" `
  '*S-1-5-18:(OI)(CI)F'
& icacls.exe $tokenFile `
  '/inheritance:r' `
  '/grant:r' `
  "${user}:F" `
  '*S-1-5-18:F'
```

### TTS service の起動

動作確認中は foreground で起動する。

```powershell
cd C:\src\momo\tools\race-audio-service
$tokenFile = Join-Path $env:LOCALAPPDATA 'MomoFPV\secrets\race-audio-service-token.txt'
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = [IO.File]::ReadAllText($tokenFile).Trim()
uv run python .\race_audio_service.py `
  --listen 127.0.0.1:18090 `
  --engine kokoro
```

通常運用で terminal を占有しない場合は hidden process として起動する。

```powershell
$serviceDirectory = 'C:\src\momo\tools\race-audio-service'
$runtimeDirectory = Join-Path $env:LOCALAPPDATA 'MomoFPV'
$logDirectory = Join-Path $runtimeDirectory 'logs'
$tokenFile = Join-Path $runtimeDirectory 'secrets\race-audio-service-token.txt'
New-Item -ItemType Directory -Force -Path $logDirectory | Out-Null
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = [IO.File]::ReadAllText($tokenFile).Trim()
$process = Start-Process `
  -FilePath (Get-Command uv).Source `
  -ArgumentList 'run','python','.\race_audio_service.py','--listen','127.0.0.1:18090','--engine','kokoro' `
  -WorkingDirectory $serviceDirectory `
  -WindowStyle Hidden `
  -RedirectStandardOutput (Join-Path $logDirectory 'race-audio-service.stdout.log') `
  -RedirectStandardError (Join-Path $logDirectory 'race-audio-service.stderr.log') `
  -PassThru
Remove-Item Env:MOMO_RACE_AUDIO_SERVICE_TOKEN
[IO.File]::WriteAllText(
  (Join-Path $runtimeDirectory 'race-audio-service.pid'),
  [string]$process.Id
)
```

Kokoro の model load と warmup に数秒かかる。Relay より先に health check を通す。

```powershell
Invoke-RestMethod http://127.0.0.1:18090/healthz
```

### 同居 Relay の起動

Relay を起動する PowerShell で同じ token file を読み、service URL を loopback にする。

```powershell
$tokenFile = Join-Path $env:LOCALAPPDATA 'MomoFPV\secrets\race-audio-service-token.txt'
$env:MOMO_RACE_AUDIO_SERVICE_URL = 'http://127.0.0.1:18090'
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = [IO.File]::ReadAllText($tokenFile).Trim()
```

この environment を設定した同じ PowerShell から、通常の Relay 起動コマンドまたは
`tools/start-mads-observer.ps1` を実行する。Relay 起動後に environment を設定しても反映されない。

現在の hidden process 起動は Windows 再起動後に自動復帰しない。本番常用前に Task Scheduler または
Windows service として TTS の起動、health check、Relay の順序を固定する。

## Relay の適用

`11.100` へ同じ token を安全な経路で渡し、Relay を起動する PowerShell process の environment に設定する。
別 terminal で設定した environment は、すでに起動している Relay には反映されない。

```powershell
$env:MOMO_RACE_AUDIO_SERVICE_URL = 'http://192.168.11.105:18090'
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = '<TTS service と同じ token>'
```

その状態で更新済み Relay binary と Viewer web assets を配置し、Relay を再起動する。
`tools/start-mads-observer.ps1` は URL が設定されている場合に次を自動で渡す。

```text
-race-audio-service-url http://192.168.11.105:18090
-race-audio-default-language en-US
-race-audio-en-voice am_michael
-race-audio-speed 1.04
```

Relay のビルドと再起動だけでは不十分である。TTS service の起動、URL、token、Race Control 接続を
すべて確認する。

## Viewer の適用

audio track は接続開始時の SDP に含まれる。Relay 再起動前から開いている Viewer には後付けされない。

1. Viewer を `Ctrl+F5` で強制再読み込みする。
2. `CONNECT` を押して新しい PeerConnection を作る。
3. `raceAnnounce=0` が URL にないことを確認する。
4. LAP 完了と GOAL で固定 cue、生成音声、M5Audio ducking を確認する。

正常時の Viewer 診断ログ:

```text
race audio dc open
race audio capabilities enabled en-US
race audio cue queued
race audio playing
```

`race audio capabilities` が出ない場合は、Relay が TTS service URL なしで起動しているか、古い binary / web
assets を使っている。`race audio cue queued` だけ出て生成音声が出ない場合は、TTS service の health、token、
Relay log の synthesis error を確認する。

## 適用確認

1. `GET /healthz` が TTS host と `11.100` の両方から成功する。
2. Relay status と Race Control の race channel が接続済みである。
3. Viewer を再接続し、`momo-race-audio` capability を確認する。
4. 新しい LAP で radio cue と LAP 音声を 1 回だけ再生する。
5. GOAL で finish 音声を 1 回だけ再生する。
6. TTS 再生中だけ M5Audio gain が 40% になり、終了後 100% へ戻る。
7. TTS service を停止しても映像、操縦、telemetry、race state が継続する。
8. LAN Pilot と Ayame Pilot の両方で確認する。

## ロールバック

Relay の `MOMO_RACE_AUDIO_SERVICE_URL` を外して再起動する。専用 audio track と capability は作られず、
Pilot は既存 Web Speech API を使う。TTS service は停止してよい。Race Control、M5Audio、FFB、車体 firmware
のロールバックは不要である。
