# Venue camera and Program Director

Status: doing

## Context

観客映像は全車の車載映像を常時並べるのではなく、HDMI captureした俯瞰カメラを基準映像とし、
上位、接戦、PIT、重大eventの車載映像をmainまたはwipeへ切り替える。俯瞰カメラをProgram PCの
local deviceとして直接開くとPC移設と遠隔表示を制約するため、最初から独立Momo publisherとして
Relayへ接続する。

MADSYSTEM向けBGRA shared frameは移行期の表示経路であり、Program映像の入力には使用しない。

## Goal

Relay source registryが`vehicle`と`venue`を区別し、Program Observerが俯瞰1本を常時接続しながら、
Auto Directorまたは手動操作で少数の車載映像だけをactive/warm接続できる土台を作る。

## Acceptance Criteria

- USB HDMI Captureを入力にしたMomo publisherを`venue-main`としてRelayへ登録できる。
- `venue` sourceは`raceCarId`、DRIVE参加、Marker Node assignment、PIT/HP、Pilot ticketを持たない。
- Operations APIとProgram Observerが`sourceKind`と表示名を取得できる。
- Program ObserverはTrack全画面、Track + 2 onboard wipe、Onboard main + Track PinPを切り替えられる。
- 接続上限の初期値はvenue pinned 1、vehicle active最大2、vehicle warm最大1とする。
- Auto Director停止時はTrack固定、Track停止時はleader onboardへfallbackする。
- 映像選択やProgram画面停止がPilot DataChannel、Marker Observer、Timing Engineへ影響しない。
- MADSYSTEM向け`Local\MomoObserverFrameV1`をProgram経路へ依存させない。

## Verification

- Relay config/source registry schemaとAPI fixture test
- venue sourceへのPilot/DRIVE/marker assignment拒否test
- HDMI capture MomoからRelayへのWebRTC接続test
- Relayとは別PCのProgram ObserverでTrack映像を表示
- Track、Battle wipe、Onboard mainのmanual切り替えと接続数を確認
- venue切断、復帰、Auto Director停止時のfallback test
- 切り替え前後のPilot RTT、Relay CPU、Program decode FPS、黒画面時間を記録

## Notes

- 初期Program出力はbrowserとし、配信・録画はOBS Browser Sourceを使用する。
- Auto Directorは映像解析を行わず、Race Control state、PIT、確定eventからlayoutを選ぶ。
- 全車のstate/event購読と少数の映像購読を分離してから自動切り替えを有効にする。
- 詳細設計は`doc/SCALABLE_MARKER_AND_PROGRAM_OBSERVER_DESIGN.md`を正とする。
- Relay configと動的source registryへ`sourceKind=vehicle|venue`と`displayName`を追加した。
  省略時は既存sourceを`vehicle`として扱う。
- `venue`はRace Control fan-out、Garage、Ayame Pilot、Pilot WebSocketから除外し、Operations APIから
  source kindと表示名を確認できる。Marker Node assignmentの除外はNode側設定で維持する。
- 現行RelayのH.264 capabilityはlevel 3.1であり、FHD 30 FPS俯瞰映像にはlevel 4.0対応が必要である。
  HDMI captureの実映像試験は今回保留し、source control-planeだけを先に固定した。
- 現行Relayは通常の上流WebRTC音声trackをfan-outしない。会場音は映像source登録とは別の後続契約とする。
- 2026-08-17: Relay Go test全件を実行し、`venue` schema、Operations identity、Garage除外、
  Pilot接続拒否、Observer command拒否を確認した。
