# M5 S3 binary telemetry V3

## 目的

`115200 8N1` の Pi UART で音声と IMU telemetry を併用しつつ、Compact state を 30 Hz で安定して送る。UART 区間だけを binary 化し、Pi の Momo が既存の `TEL:` JSON text DataChannel message へ復元する。Relay、Observer、Viewer の telemetry 契約は変更しない。

## 非対象

- RAW V1 の診断用途を置き換えない。
- WebRTC DataChannel の binary 化をしない。
- UART baud rate をこの段階では変更しない。

## UART framing

binary frame は `0x00 + COBS(payload) + 0x00` とする。既存の `TEL:` / `AUD:` は LF 終端 ASCII のまま混在できる。Pi parser は先頭 `0x00` を binary frame 開始として扱い、COBS decode と CRC16-CCITT (`init=0xffff`, polynomial=`0x1021`) が成功した frame だけを受理する。

payload は little-endian、末尾 2 bytes が CRC16 である。

| Field | IMU State | IMU Event | ESC State |
| --- | ---: | ---: | ---: |
| version | `3` u8 | `3` u8 | `3` u8 |
| type | `1` u8 | `2` u8 | `3` u8 |
| flags | FLU axis bit u8 | FLU axis bit u8 | `0` u8 |
| protocol | reserved u8 | reserved u8 | `1` = BL-RS4 PRG u8 |
| boot | u32 | u32 | u32 |
| sequence | u32 | u32 | u32。IMU とは独立採番 |
| timestamp_us | u64 | u64 | ESC 応答受信時刻 u64 |
| state / event fields | accel FLU i16 × 3, yaw i16, period u32 | magnitude u16, axis FLU i16 × 3, jerk u16 | 下表参照 |
| crc | u16 | u16 | u16 |

加速度は `0.01 m/s²`、yaw は `0.01 rad/s`、event magnitude は `0.1 m/s²`、axis は `0.001`、jerk は `1 m/s³` 単位とする。state payload は 34 bytes、COBS framing を含めても約 37 bytes である。

### ESC State type 3

ESC payload は 48 bytes 固定とする。

| Offset | Field | Type | Unit |
| ---: | --- | --- | --- |
| 0 | version | u8 | `3` |
| 1 | type | u8 | `3` |
| 2 | flags | u8 | `0` |
| 3 | protocol | u8 | `1` = BL-RS4 PRG |
| 4 | boot | u32 | M5 起動 ID |
| 8 | sequence | u32 | ESC 応答ごとの独立連番 |
| 12 | timestamp_us | u64 | ESC 応答受信時刻 |
| 20 | valid_mask | u16 | bit0 RPM、bit1 最大 RPM、bit2 電圧、bit3 温度、bit4 出力率 |
| 22 | status_flags | u16 | bit0 fresh |
| 24 | motor_rpm | u32 | rpm |
| 28 | maximum_motor_rpm | u32 | rpm |
| 32 | voltage | u16 | mV |
| 34 | current | u16 | 0.01 A。未解析時は valid に含めない |
| 36 | temperature | i16 | 0.1 °C |
| 38 | consumed_capacity | u16 | mAh。未解析時は valid に含めない |
| 40 | drive_output | u16 | 0 ～ 1000 |
| 42 | response_age | u16 | ms |
| 44 | poll_period | u16 | ms |
| 46 | crc | u16 | CRC16-CCITT |

Momo は type 3 を次の V2 state へ正規化する。未解析 field は `esc` から省略し、`imu0` と同じ `src` にまとめない。

```json
{"v":2,"k":"s","src":"esc0","boot":"7f3a21c4","seq":3,"t_us":1200000,"esc":{"rpm":12400,"max":18000,"v":8.200,"tc":29.0,"out":430},"q":{"p":200000,"ok":true,"age":12,"f":["blrs4_prg"]}}
```

## 優先順位と周期

- `BIN` state は 30 Hz (`33333 us`)。
- ESC は BL-RS4 を 5 Hz で poll し、応答取得時だけ type 3 state を送る。
- `impact_candidate` event は pending 時に state より先に送る。
- audio の frame と binary telemetry は同じ UART を使うが、state は送信バッファ不足時に drop して待たない。
- event は buffer 不足時に次 loop で再試行する。送信成功まで pending を維持する。
- 送信優先順位は impact event、audio、IMU state、ESC state とする。ESC state はバッファ不足時に drop し、RC command と audio を待たせない。

30 Hz state は約 `1.1 KB/s`。現行 audio 約 `7.25 KB/s` と合計しても `115200 8N1` の実効約 `11.52 KB/s` 未満に収める。

## 互換性と切替

M5 S3 の telemetry mode は `RAW`、`CMP`、`BIN` の 3 種類にする。

- `RAW`: 従来 V1 JSON、音声併用 15 Hz、診断用。
- `CMP`: 従来 V2 JSON、音声併用 20 Hz、旧 Momo との互換用。
- `BIN`: V3 binary UART、30 Hz、binary 対応 Momo が必要。

Momo が V3 frame を復元した後に出す DataChannel payload は既存 V2 JSON と同じにする。V3 非対応の Momo で `BIN` を選んだ場合、RC 操作は継続するが telemetry と IMU audio の UART parser は保証しない。そのため Pi 側 Momo を先に更新する。

## 受入条件

- audio 有効かつ `BIN` 30 Hz で state drop が継続増加しない。
- Relay / Observer / Pilot Viewer が V2 `TEL:` として既存どおり telemetry を表示する。
- `impact_candidate` が Viewer OSD と FFB Bridge へ届く。
- 10 分走行で telemetry sequence の連続的な欠落、audio の連続断、映像 FPS 低下を起こさない。
