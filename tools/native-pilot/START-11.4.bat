@echo off
cd /d "%~dp0"
momo.exe --fullscreen p2p-pilot --no-google-stun --label 11.4 --endpoint "ws://192.168.11.104:8090/ws?role=pilot&device=11.4" --input-config input-11.4.json --flip-vertical --flip-horizontal
