# Native Pilot

`START-11.4.bat` または `START-11.5.bat` を実行する。

起動時はフルスクリーンで表示する。`F` キーでフルスクリーンを切り替え、`Q` キーで終了する。

別 PC で映像が黒いままの場合は、最初に `ALLOW_PRIVATE_NETWORK.bat` を実行する。これは `momo.exe` の UDP / TCP 受信を Windows Firewall の Private ネットワークだけで許可する。Public ネットワークには許可しない。

起動直後は Drive Off で、ニュートラルのみを送信する。`input-11.x.json` の `driveButton` を押すと Drive On / Off が切り替わる。ハンコンが切断された場合は Drive Off に戻る。

入力 JSON は relay Web Pilot の Input 画面で作るマッピングと同じキー名を使う。Web Input の「設定をコピー」で得た JSON を対象機体の `input-11.x.json` へ貼り付けると、軸のキャリブレーション値を転記できる。
