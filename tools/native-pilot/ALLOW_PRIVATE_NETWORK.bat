@echo off
net session >nul 2>&1
if not "%errorlevel%"=="0" (
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -Verb RunAs"
  exit /b
)

netsh advfirewall firewall add rule name="Momo Native Pilot UDP" dir=in action=allow program="%~dp0momo.exe" enable=yes profile=private protocol=UDP
netsh advfirewall firewall add rule name="Momo Native Pilot TCP" dir=in action=allow program="%~dp0momo.exe" enable=yes profile=private protocol=TCP
echo Momo Native Pilot has been allowed on Private networks.
pause
