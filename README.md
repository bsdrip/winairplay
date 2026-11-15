# winairplay

winairplay mirrors the screen and the sound of a Windows PC to an Apple TV
with the AirPlay protocol. It runs as an icon in the notification area of the
taskbar.

## Install

1. Open the [Releases](../../releases) page and download
   `winairplay-setup-<version>.exe`.
2. Run it. Windows SmartScreen shows a warning, because the installer has no
   code signature. Select **More info**, then **Run anyway**.
3. Start **winairplay** from the Start menu.

The installer needs no administrator rights. It includes FFmpeg, which
winairplay uses to capture and encode the screen.

## Use

1. Click the screen icon in the notification area. If you do not see it,
   click the arrow (`^`) next to the clock.
2. When Windows Firewall asks, allow access on private networks.
3. Click **Mirror to _name of your Apple TV_**.
4. The first time, the Apple TV shows a PIN. Type it into the window that
   winairplay opens.
5. The icon becomes blue when mirroring is active. The first line of the menu
   shows the picture size, for example `3840 x 2160 HEVC`.
6. To stop, click the icon and click **Stop mirroring**.

winairplay searches the network for receivers continuously (mDNS). If that
finds nothing, it also tries each address of the local network on the AirPlay
port.

### Menu

A changed setting applies to the next session.

| Menu item | Function |
| --- | --- |
| Mirror to an IP address... | Connects to an address that you type. |
| Search again | Searches for receivers immediately. |
| Display | Selects the monitor (only with more than one monitor). |
| Picture | **4K**: HEVC video at up to 3840 x 2160. **1080p**: H.264 video. |
| Video encoder | **Automatic** is correct for most computers. Select NVIDIA, Intel or AMD to force one graphics card encoder. |
| Frame rate | 15, 30 or 60 frames per second. |
| Show the mouse pointer | Includes the pointer in the picture. |
| Sound | Selects the TV, the PC, or the two. See [Sound](#sound). |
| Write a detailed log | Adds the protocol messages to the log file. |
| Open the log file | Opens `%LocalAppData%\winairplay\winairplay.log`. |

### 4K

4K needs these three things:

- An Apple TV 4K that is set to a 4K output format.
- A graphics card with an HEVC encoder (NVIDIA, Intel or AMD from
  approximately 2016 or later). **Automatic** tests the encoders and uses the
  first one that operates.
- A good network connection: winairplay sends approximately 25 Mbit/s at 30
  frames per second. A network cable to the Apple TV is best.

If 4K cannot start, winairplay shows a notice and uses 1080p.

### Sound

The **Sound** menu selects where you hear the sound of the PC. You can
change it during mirroring; the change is immediate.

| Selection | Result |
| --- | --- |
| TV and this PC | The TV and the PC (headphones or speakers) play the sound. |
| TV only (mutes this PC) | The TV plays the sound. winairplay mutes the output device of the PC and removes the mute when you change the selection or stop mirroring. |
| This PC only | Only the PC plays the sound. The TV is silent. |

winairplay sends the sound of the default output device of Windows.

With some audio drivers, a muted device gives no sound to winairplay. If
the TV is silent with **TV only**, select **TV and this PC** and decrease
the volume at the headphones or speakers.

## Troubleshooting

| Symptom | Possible cause and remedy |
| --- | --- |
| The receiver list is empty | Click **Search again**. If the list stays empty, use **Mirror to an IP address...**. Some guest and corporate networks isolate the devices from each other. |
| Pairing fails | On the Apple TV, open Settings > AirPlay and HomeKit. Set **Allow Access** to "Anyone on the Same Network". Then try again. |
| The session stops after some seconds | Allow `winairplay.exe` in Windows Firewall for private networks. |
| Notice "4K is not available" | Read the reason in the notice. See [4K](#4k). |
| The 4K picture stutters | Select 30 frames per second, use a network cable, or select **1080p**. |
| No sound on the TV | In the **Sound** menu, select **TV and this PC**. Make sure that the sound plays on the default output device of Windows. |
| The PC stays muted after winairplay was stopped by force | Remove the mute with the Windows volume control. |
| The picture is black for protected video | Windows does not let programs capture DRM-protected video. This is normal. |

For other problems, select **Write a detailed log**, repeat the steps, and
read the log file.

## Build from source

Requirements: Go 1.24 or later. To run the program, FFmpeg 6.0 or later must
be on `PATH` or next to `winairplay.exe`.

```powershell
powershell -ExecutionPolicy Bypass -File build.ps1
```

GitHub Actions builds the installer (`.github/workflows/build.yml`). To make
a release, push a tag: `git tag v1.2.3` and `git push origin v1.2.3`.

## Test status

| Item | Status |
| --- | --- |
| Mirroring from Windows to a real Apple TV, 1080p H.264 | Confirmed by a user with version 0.1. |
| 4K HEVC, sound, network scan | Automated tests pass against a software test receiver. Not yet confirmed with a real Apple TV. |
| Hardware HEVC encoders (NVIDIA, Intel, AMD) | Not tested: the build servers have no graphics card. |
| Installer | Each build installs, starts and uninstalls the program on Windows. |

Run the tests with `go test ./...` on Linux. The session tests need FFmpeg
and are skipped without it. Many tests of `internal/airplay` come from
doubletake and need Linux.

## Limits

- One receiver at a time.
- No HDR.
- The sound comes from the default output device only.

## Structure

| Path | Contents |
| --- | --- |
| `internal/airplay` | AirPlay protocol (from doubletake), FFmpeg capture and sound capture. |
| `internal/mirror` | One mirroring session, and the search for receivers. |
| `cmd/winairplay` | Tray application (Win32 API, no GUI library). |
| `cmd/winairplay-test-receiver` | Software receiver for tests (from doubletake). It is not in the installer. |
| `installer` | Inno Setup script and icon. |

## License and origin

winairplay is licensed under the LGPL-3.0-or-later. The AirPlay protocol
code, which includes the FairPlay handshake, comes from
[doubletake](https://github.com/omarroth/doubletake) by Omar Roth. See
[NOTICE](NOTICE) for the list of changes and for the FFmpeg license.

The FairPlay handshake is used here only for interoperability: to show your
own screen on your own Apple TV. winairplay does not decrypt or copy protected
media.

AirPlay, Apple TV and FairPlay are trademarks of Apple Inc. This project is
not affiliated with Apple Inc.
