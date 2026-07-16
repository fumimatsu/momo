@echo off
cd /d "%~dp0"
momo.exe --fullscreen --no-google-stun p2p-pilot --label 11.4 --endpoint "ws://192.168.11.104:8090/ws?role=pilot&device=11.4" --input-config input-11.4.json --flip-vertical --flip-horizontal
