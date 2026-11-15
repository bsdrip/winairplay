# winairplay

winairplay mirrors the screen of a Windows PC to an Apple TV with the AirPlay
protocol. It runs as an icon in the notification area of the taskbar.

**Status: first version. Video only, no sound.** The automated tests pass
against a software test receiver. The program is not yet tested with a real
Apple TV. See [Test status](#test-status).

## What it does

- Finds the AirPlay receivers on the local network (mDNS).
- Pairs with the Apple TV. The Apple TV shows a PIN one time; winairplay saves
  the pairing keys in the Windows Credential Manager.
- Does the FairPlay handshake that an Apple TV requires for screen mirroring.
- Captures the screen with FFmpeg (Desktop Duplication API), encodes H.264 and
  sends the encrypted video stream.

## Requirements

- Windows 10 or Windows 11, 64-bit.
- FFmpeg 6.0 or later with the `ddagrab` filter and an H.264 encoder. The
  builds from gyan.dev and BtbN include them.
  - Install it with `winget install Gyan.FFmpeg`, or
  - put `ffmpeg.exe` in the same directory as `winairplay.exe`.
- The PC and the Apple TV must be on the same network.
- To build: Go 1.24 or later.

## Build

```powershell
powershell -ExecutionPolicy Bypass -File build.ps1
```

The script writes three programs into `bin`:

| Program | Function |
| --- | --- |
| `winairplay.exe` | The tray application. |
| `winairplay-cli.exe` | The same function on the command line, with a full log. |
| `winairplay-test-receiver.exe` | A software AirPlay receiver for tests. It shows no picture. |

On Linux or macOS, `make windows` cross-compiles the same programs.

## Use the tray application

1. Start `winairplay.exe`. An icon of a screen appears in the notification
   area. If you do not see it, click the arrow (`^`) next to the clock.
2. When Windows Firewall asks, allow access on private networks. The Apple TV
   must be able to send timing packets to the PC.
3. Click the icon. Click **Mirror to _name of your Apple TV_**.
4. The first time, the Apple TV shows a PIN. Type it into the window that
   winairplay opens.
5. The icon becomes blue when mirroring is active.
6. To stop, click the icon and click **Stop mirroring**.

The menu also has these settings. A changed setting applies to the next
session.

| Menu item | Function |
| --- | --- |
| Mirror to an IP address... | Connects without discovery. Use it if the list stays empty. |
| Display | Selects the monitor (only with more than one monitor). |
| Video encoder | Automatic uses the x264 software encoder. Select NVIDIA, Intel or AMD to use the graphics hardware. |
| Frame rate | 15, 30 or 60 frames per second. |
| Show the mouse pointer | Includes the pointer in the picture. |
| Open the log file | Opens `%LocalAppData%\winairplay\winairplay.log`. |

## Use the command line program

```powershell
bin\winairplay-cli.exe -list                      # show the receivers
bin\winairplay-cli.exe -name "Living Room"        # mirror to a receiver
bin\winairplay-cli.exe -target 192.168.1.50       # mirror to an address
bin\winairplay-cli.exe -name "Living Room" -test  # send a test pattern
bin\winairplay-cli.exe -name "Living Room" -debug # full protocol log
```

Press Ctrl+C to stop. `winairplay-cli.exe -help` shows all options.

## Troubleshooting

| Symptom | Possible cause and remedy |
| --- | --- |
| "FFmpeg not found" | Install FFmpeg or put `ffmpeg.exe` next to `winairplay.exe`. |
| The receiver list is empty | The firewall or the network blocks mDNS (UDP port 5353). Use **Mirror to an IP address...**. Some guest and corporate networks isolate the devices from each other. |
| Pairing fails | On the Apple TV, open Settings > AirPlay and HomeKit. Set **Allow Access** to "Anyone on the Same Network". Then try again. |
| The session stops after some seconds | Allow `winairplay.exe` in Windows Firewall for private networks. |
| The picture is black for protected video | Windows does not let programs capture DRM-protected video. This is normal. |
| A hardware encoder fails | Select **Automatic (software)**. Then read the FFmpeg messages in the log file. |

If a problem continues, run `winairplay-cli.exe` with `-debug` and read the
output.

## Test status

| Item | Status |
| --- | --- |
| Protocol code from doubletake | Upstream reports operation with Apple TV 4K (AppleTV11,1 and AppleTV14,1) from Linux. |
| Full session on loopback (pairing, FairPlay, encrypted video from FFmpeg) | Automated test, passes on Linux. |
| Windows build | Compiles. `go vet` reports no problems. |
| Tray application on Windows | Not tested. |
| Screen capture with `ddagrab` on Windows | Not tested. |
| Mirroring to a real Apple TV from Windows | Not tested. |

Run the tests with `go test ./...` on Linux. The session tests need FFmpeg
with libx264 and are skipped without it. The tests of `internal/airplay` come
from doubletake and many of them need Linux; they were not run on Windows.

## Limits

- No sound. Audio capture (WASAPI loopback) is not implemented.
- H.264 only. The receiver scales the picture to its screen; winairplay sends
  at most the canvas size that the receiver gives (usually 1920 x 1080).
- One receiver at a time.

## Structure

| Path | Contents |
| --- | --- |
| `internal/airplay` | AirPlay protocol (from doubletake) and the FFmpeg capture backend. |
| `internal/mirror` | One mirroring session: connect, pair, FairPlay, capture, stream. |
| `cmd/winairplay` | Tray application (Win32 API, no GUI library). |
| `cmd/winairplay-cli` | Command line program. |
| `cmd/winairplay-test-receiver` | Software receiver for tests (from doubletake). |

## License and origin

winairplay is licensed under the LGPL-3.0-or-later. The AirPlay protocol
code, which includes the FairPlay handshake, comes from
[doubletake](https://github.com/omarroth/doubletake) by Omar Roth. See
[NOTICE](NOTICE) for the list of changes.

The FairPlay handshake is used here only for interoperability: to show your
own screen on your own Apple TV. winairplay does not decrypt or copy protected
media.

AirPlay, Apple TV and FairPlay are trademarks of Apple Inc. This project is
not affiliated with Apple Inc.
