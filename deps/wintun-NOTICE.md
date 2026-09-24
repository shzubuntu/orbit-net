# Third-party component: Wintun

`wintun.dll` is the Wintun TUN device driver, authored by WireGuard LLC.

- Upstream: https://www.wintun.net/builds (prebuilt binary, v0.14.x era, amd64)
- Copyright: 2018-2021 WireGuard LLC. All Rights Reserved.
- License: **WireGuard LLC "Prebuilt Binaries License"** — see `wintun-LICENSE.txt`
  (shipped alongside the DLL in the Windows client package).

Important for this open-source project:

- The binary is NOT open source. The full license text must accompany every
  redistribution of `wintun.dll`.
- It is redistributable ONLY when shipped together with software that uses the
  DLL strictly via the "Permitted API" (`wintun.h`), which is how `orbit-cli`
  consumes it. Do NOT modify, reverse engineer, or re-sell the DLL on its own.
- Consequently the DLL is NOT committed to this repository. The Windows release
  package is assembled by `deploy/build-orbit-win.sh` on the operator's machine
  (it pulls `wintun.dll` from `/opt/orbit/www/`) and bundles both this notice
  and the full license text.
- `wintun-LICENSE.txt` is the verbatim upstream license text (kept here so the
  open build doc stays self-describing).