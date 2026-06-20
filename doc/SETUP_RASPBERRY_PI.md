# Raspberry Pi (Raspberry-Pi-OS) で Momo を使ってみる

## 注意

Raspberry Pi OS のレガシー版には対応しておりません。最新版の Raspberry Pi OS を利用してください

## Raspberry Pi 向けのバイナリは以下にて提供しています

<https://github.com/shiguredo/momo/releases> にて最新版のバイナリをダウンロードしてください。

- Raspberry Pi OS 64 bit を利用する場合は、 `momo-<VERSION>_raspberry-pi-os_armv8.tar.gz` を利用してください

## ダウンロードしたパッケージ、解凍後の構成

```console
$ tree
.
├── html
│   ├── p2p.html
│   └── webrtc.js
├── LICENSE
├── momo
└── NOTICE
```

## 準備

### パッケージのインストール

下記を実行してください。

```bash
sudo apt-get update
sudo apt-get upgrade
sudo apt-get install libnspr4 libnss3
sudo apt-get install libcamera0.6
```

#### Raspberry Pi OS Lite を利用する場合

Raspberry Pi Lite では映像に関するパッケージが入っていないため、`ldd ./momo | grep not` を実行し、不足しているパッケージを確認してください。

下記に実行する一例を示します。

```bash
sudo apt-get install libxtst6
sudo apt-get install libegl1-mesa-dev
sudo apt-get install libgles2-mesa
```

### FPV 向けの基本設定

Raspberry Pi Zero 2 W と Raspberry Pi 4B を FPV 用途で利用する場合は、遅延の揺れを減らすために以下の設定を行ってください。

- GUI を無効化して CUI 起動にする
- Wi-Fi の省電力を無効化する
- CPU governor を `performance` に固定する
- 不要な NFS / rpcbind / Bluetooth サービスを停止する
- `momo.service` を `network-online.target` 後に起動する
- `.local` 名で接続したい場合は `avahi-daemon` を残す

GUI を無効化します。

```bash
sudo systemctl set-default multi-user.target
sudo systemctl disable --now display-manager.service 2>/dev/null || true
sudo systemctl disable --now lightdm.service 2>/dev/null || true
```

NetworkManager を利用している場合は、接続名を確認して Wi-Fi の省電力を無効化します。IPv4 固定運用で IPv6 を利用しない場合は IPv6 も無効化します。

```bash
nmcli -g NAME,TYPE,DEVICE connection show --active
sudo nmcli connection modify "<接続名>" 802-11-wireless.powersave 2 ipv6.method disabled
sudo nmcli connection reload
```

CPU governor を `performance` に固定します。

```bash
sudo tee /etc/systemd/system/cpu-performance.service >/dev/null <<'EOF'
[Unit]
Description=Set CPU governor to performance for FPV
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'for governor in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do [ -w "$governor" ] && echo performance > "$governor"; done'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now cpu-performance.service
```

FPV 専用機で利用しないサービスを停止します。`momo-fpv-10.local` のような `.local` 名を使う場合は `avahi-daemon` を停止しないでください。

```bash
sudo systemctl disable --now nfs-blkmap rpcbind bluetooth 2>/dev/null || true
```

`momo.service` を `network-online.target` 後に起動するようにします。

```bash
sudo mkdir -p /etc/systemd/system/momo.service.d
sudo tee /etc/systemd/system/momo.service.d/30-network-online.conf >/dev/null <<'EOF'
[Unit]
After=network-online.target
Wants=network-online.target
EOF
sudo systemctl daemon-reload
```

設定後に再起動して、以下のようになっていることを確認してください。

```bash
sudo reboot
```

```bash
systemctl get-default
systemctl is-active display-manager 2>/dev/null || true
systemctl is-active lightdm 2>/dev/null || true
systemctl is-enabled avahi-daemon 2>/dev/null || true
systemctl is-enabled nfs-blkmap rpcbind bluetooth 2>/dev/null || true
cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor
nmcli -g 802-11-wireless.powersave,ipv6.method connection show "<接続名>"
```

期待する状態は以下です。

```text
multi-user.target
inactive
inactive
enabled
disabled
disabled
disabled
performance
disable
disabled
```

### Raspberry-Pi-OS で Raspberry Pi 用カメラなどの CSI カメラを利用する場合

これは USB カメラを利用する場合は不要なオプションです。

raspi-config で Camera を Enable にしてください。

加えて、以下のコマンドを実行してください

```bash
sudo modprobe bcm2835-v4l2 max_video_width=2592 max_video_height=1944
```

## 使ってみる

[USE_P2P.md](USE_P2P.md) をご確認ください。

## ビデオデバイスの指定

ビデオデバイスの指定については [LINUX_VIDEO_DEVICE.md](LINUX_VIDEO_DEVICE.md) をご確認ください。

## Raspberry Pi 向けの追加のオプション

### --force-i420

`--force-i420` は Raspberry Pi 専用カメラ用では MJPEG を使うとパフォーマンスが落ちるため HD 以上の解像度でも MJPEG にせず強制的に I420 でキャプチャーします。
USB カメラでは逆にフレームレートが落ちるため使わないでください。

```bash
./momo --force-i420 --no-audio-device p2p
```

## Raspberry Pi 専用カメラでが利用できない

Momo 2023.1.0 から Raspberry Pi OS (64 bit) でのみ Raspberry Pi 専用カメラ（CSI 接続のカメラ）が利用できるようになりました。

### --use-libcamera

`--use-libcamera` は Raspberry Pi 専用カメラを利用するためのオプションです。

```bash
./momo --use-libcamera --no-audio-device p2p
```

## Raspberry Pi 専用カメラでパフォーマンスが出ない

### --hw-mjpeg-decoder

MJPEG のハードウェアデコーダーの利用を検討してみてください。
`--hw-mjpeg-decoder` は ハードウェアによるビデオのリサイズをします。

```bash
./momo --hw-mjpeg-decoder true --no-audio-device p2p
```

### Raspberry Pi の設定を見直す

[Raspberry-Pi-OS で Raspberry Pi 用カメラなどの CSI カメラを利用する場合](#raspberry-pi-os-で-raspberry-pi-用カメラなどの-csi-カメラを利用する場合) を確認してください。
特に `max_video_width=2592 max_video_height=1944` が記載されていなければ高解像度時にフレームレートが出ません。

### オプションを見直す

Raspberry Pi 用カメラ利用時には `--hw-mjpeg-decoder=true --force-i420` オプションを併用すると CPU 使用率が下がりフレームレートが上がります。例えば、 Raspberry Pi Zero の場合には

```bash
./momo --resolution=HD --force-i420 --hw-mjpeg-decoder=true p2p
```

がリアルタイムでの最高解像度設定となります。

## USB カメラでパフォーマンスが出ない

### --hw-mjpeg-decoder

一部の MJPEG に対応した USB カメラを使用している場合、 `--hw-mjpeg-decoder` は ハードウェアによるビデオのリサイズ と MJPEG をハードウェアデコードします。

```bash
./momo --hw-mjpeg-decoder true --no-audio-device p2p
```

### Raspberry Pi で USB カメラ利用時に --hw-mjpeg-decoder を使ってもフレームレートが出ない

USB カメラ利用時にフレームレートを出したい場合は `--hw-mjpeg-decoder` を使わないことをおすすめします。ただし CPU 使用率はあがってしまいます。

CPU 使用率を抑えつつフレームレートを出したい場合は、`/boot/firmware/config.txt` の末尾に下記を追記して `--hw-mjpeg-decoder` を指定することで改善することがあります。

bookworm より前のバージョンをご利用の場合は `/boot/config.txt` に追記してください。

```text
gpu_mem=256
force_turbo=1
avoid_warnings=2
```

この設定であれば HD は 30fps, FHD では 15fps 程度の性能を発揮します。
