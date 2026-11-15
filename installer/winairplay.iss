; Inno Setup script for winairplay.
; Build:  ISCC /DAppVersion=1.2.3 installer\winairplay.iss
; Input:  dist\app\winairplay.exe, dist\ffmpeg\ffmpeg.exe, dist\ffmpeg\LICENSE
; Output: dist\winairplay-setup-<version>.exe

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif

[Setup]
AppId={{6B1D2C0E-7B0E-4E0B-9A53-5E3C1B7F2A41}
AppName=winairplay
AppVersion={#AppVersion}
AppPublisher=bsdrip
AppPublisherURL=https://github.com/bsdrip/winairplay
DefaultDirName={autopf}\winairplay
DisableProgramGroupPage=yes
DisableDirPage=auto
; The program is installed for the current user. No administrator rights.
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir=..\dist
OutputBaseFilename=winairplay-setup-{#AppVersion}
SetupIconFile=winairplay.ico
UninstallDisplayIcon={app}\winairplay.ico
UninstallDisplayName=winairplay
LicenseFile=..\LICENSE
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
CloseApplications=no

[Tasks]
Name: "autostart"; Description: "Start winairplay when I sign in to Windows"; Flags: unchecked

[Files]
Source: "..\dist\app\winairplay.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\dist\ffmpeg\ffmpeg.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\dist\ffmpeg\LICENSE"; DestDir: "{app}"; DestName: "FFMPEG-LICENSE.txt"; Flags: ignoreversion
Source: "winairplay.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\LICENSE"; DestDir: "{app}"; DestName: "LICENSE.txt"; Flags: ignoreversion
Source: "..\COPYING.GPL"; DestDir: "{app}"; DestName: "COPYING-GPL.txt"; Flags: ignoreversion
Source: "..\NOTICE"; DestDir: "{app}"; DestName: "NOTICE.txt"; Flags: ignoreversion
Source: "..\README.md"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\winairplay"; Filename: "{app}\winairplay.exe"; IconFilename: "{app}\winairplay.ico"

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "winairplay"; ValueData: """{app}\winairplay.exe"" --background"; Tasks: autostart; Flags: uninsdeletevalue

[Run]
Filename: "{app}\winairplay.exe"; Description: "Start winairplay now"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM winairplay.exe"; Flags: runhidden; RunOnceId: "StopWinairplay"

[Code]
// Stop a running winairplay before the files are replaced. The program has
// no visible window, so Setup cannot ask the user to close it.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ResultCode: Integer;
begin
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /IM winairplay.exe', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  Result := '';
end;
