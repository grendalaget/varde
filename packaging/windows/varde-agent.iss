; Varde — Inno Setup installer.
; Installs the agent service, mesh and tray into Program Files, starts the
; service and opens the tray's link window. The PC joins the hosted Varde
; (varde.games) unless /CPURL names a self-hosted control plane.
;
;   iscc /DVersion=0.1.0 varde-agent.iss
; Expects alongside this script: varde.ico, wizard\*.bmp, dist\varde-agent.exe,
; dist\varde-mesh.exe, dist\varde-tray.exe, dist\MicrosoftEdgeWebview2Setup.exe
;
; Silent: VardeSetup.exe /VERYSILENT /CPURL=https://cp.example [/TASKS=autostart]

#ifndef Version
  #define Version "0.0.0-dev"
#endif

[Setup]
; AppId kept from the agent-only installer so upgrades find the old install
AppId=Varde Agent
AppName=Varde
AppVersion={#Version}
AppPublisher=Varde contributors
LicenseFile=..\..\LICENSE
DefaultDirName={autopf}\Varde
DefaultGroupName=Varde
DisableProgramGroupPage=yes
OutputDir=dist
OutputBaseFilename=VardeSetup-{#Version}
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
SetupIconFile=varde.ico
; brand art per DPI (100/125/150/200/250%); Setup picks the closest
WizardImageFile=wizard\wizard-100.bmp,wizard\wizard-125.bmp,wizard\wizard-150.bmp,wizard\wizard-200.bmp,wizard\wizard-250.bmp
WizardSmallImageFile=wizard\wizard-small-100.bmp,wizard\wizard-small-125.bmp,wizard\wizard-small-150.bmp,wizard\wizard-small-200.bmp,wizard\wizard-small-250.bmp
UninstallDisplayIcon={app}\varde.ico
CloseApplications=force

[Tasks]
Name: "autostart"; Description: "Start Varde at login (tray icon)"

[Files]
Source: "varde.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\LICENSE"; DestDir: "{app}"
Source: "dist\varde-agent.exe"; DestDir: "{app}"; Flags: ignoreversion; \
  AfterInstall: WriteServerUrl
Source: "dist\varde-mesh.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\varde-tray.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\MicrosoftEdgeWebview2Setup.exe"; DestDir: "{tmp}"; \
  Flags: deleteafterinstall; Check: NeedsWebView2

[Dirs]
Name: "{commonappdata}\Varde\identity"; Permissions: system-full admins-full
Name: "{commonappdata}\Varde"; Permissions: system-full admins-full
; service logs; readable so "Open logs folder" works for any user
Name: "{commonappdata}\Varde\logs"; Permissions: system-full admins-full users-readexec

[Registry]
; all users; each user can turn it off from the tray (HKCU opt-out)
Root: HKLM; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; \
  ValueType: string; ValueName: "Varde"; \
  ValueData: """{app}\varde-tray.exe"" --autostart"; \
  Tasks: autostart; Flags: uninsdeletevalue

[Icons]
Name: "{autoprograms}\Varde"; Filename: "{app}\varde-tray.exe"; \
  IconFilename: "{app}\varde.ico"

[Run]
Filename: "{tmp}\MicrosoftEdgeWebview2Setup.exe"; Parameters: "/silent /install"; \
  Flags: waituntilterminated; Check: NeedsWebView2; \
  StatusMsg: "Installing Microsoft Edge WebView2 (for the link window)"
Filename: "{app}\varde-agent.exe"; Parameters: "service install"; \
  Flags: runhidden waituntilterminated; \
  StatusMsg: "Registering the Varde service"
Filename: "sc.exe"; Parameters: "start VardeAgent"; \
  Flags: runhidden waituntilterminated; \
  StatusMsg: "Starting the Varde service"
Filename: "{app}\varde-tray.exe"; Parameters: "{code:TrayArgs}"; \
  Flags: postinstall nowait runasoriginaluser skipifsilent; \
  Description: "{code:TrayRunLabel}"

[UninstallRun]
Filename: "taskkill.exe"; Parameters: "/F /IM varde-tray.exe"; \
  Flags: runhidden waituntilterminated; RunOnceId: "traykill"
; waits for Stopped (final save + replication hold) before deleting
Filename: "{app}\varde-agent.exe"; Parameters: "service uninstall"; \
  Flags: runhidden waituntilterminated; RunOnceId: "svcuninstall"

[Code]
const
  WebView2Key = 'SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}';

function IsLinked(): Boolean;
begin
  Result := FileExists(ExpandConstant('{commonappdata}\Varde\config.toml'));
end;

function HasWebView2(Root: Integer; Key: String): Boolean;
var
  V: String;
begin
  Result := RegQueryStringValue(Root, Key, 'pv', V) and (V <> '') and (V <> '0.0.0.0');
end;

function NeedsWebView2(): Boolean;
begin
  Result := not (HasWebView2(HKLM32, WebView2Key) or HasWebView2(HKLM64, WebView2Key)
    or HasWebView2(HKCU, WebView2Key));
end;

function CpUrl(): String;
begin
  Result := Trim(ExpandConstant('{param:CPURL|}'));
  while (Length(Result) > 0) and (Result[Length(Result)] = '/') do
    Delete(Result, Length(Result), 1);
end;

// self-hosters pass /CPURL: the service reports this address until the PC is
// linked; the tray links with it. No param means the hosted default, so no
// server.url is written.
procedure WriteServerUrl();
var
  U: String;
begin
  U := Lowercase(CpUrl());
  if IsLinked() or (U = '') then
    Exit;
  if (Pos('https://', U) <> 1) and (Pos('http://', U) <> 1) then
    Exit;
  SaveStringToFile(ExpandConstant('{commonappdata}\Varde\server.url'), U + #13#10, False);
end;

function TrayArgs(Param: String): String;
begin
  if IsLinked() then
    Result := ''
  else
    Result := '--link';
end;

function TrayRunLabel(Param: String): String;
begin
  if IsLinked() then
    Result := 'Start the Varde tray'
  else
    Result := 'Link this PC now';
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Code: Integer;
begin
  Result := '';
  // upgrade: stop the tray and remove the old service (waits for Stopped)
  Exec('taskkill.exe', '/F /IM varde-tray.exe', '', SW_HIDE, ewWaitUntilTerminated, Code);
  if FileExists(ExpandConstant('{app}\varde-agent.exe')) then
    Exec(ExpandConstant('{app}\varde-agent.exe'), 'service uninstall', '', SW_HIDE,
      ewWaitUntilTerminated, Code);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  // upgrade with "Start Varde at login" unticked: drop the earlier Run value
  if (CurStep = ssPostInstall) and not WizardIsTaskSelected('autostart') then
    RegDeleteValue(HKLM, 'Software\Microsoft\Windows\CurrentVersion\Run', 'Varde');
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Data: String;
begin
  if CurUninstallStep <> usPostUninstall then
    Exit;
  Data := ExpandConstant('{commonappdata}\Varde');
  // kept by default: local saves and this PC's identity
  if (not UninstallSilent()) and DirExists(Data) and
     (MsgBox('Also delete Varde''s data on this PC?' + #13#10#13#10 +
       'This removes local saves and this PC''s identity in ' + Data + '. ' +
       'Copies on other machines are not affected.',
       mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDYES) then
    DelTree(Data, True, True, True);
end;
