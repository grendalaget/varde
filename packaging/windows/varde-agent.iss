; Varde — Inno Setup installer.
; Installs the agent service, mesh and tray into Program Files, starts the
; service and opens the tray's link window. The PC joins the hosted Varde
; (varde.games) unless /CPURL names a self-hosted control plane — or the
; opt-in "controlplane" task installs one on this PC (service + sqlite in
; %ProgramData%\VardeCP), which then becomes this PC's default CP.
;
;   iscc /DVersion=0.1.0 varde-agent.iss
; Expects alongside this script: varde.ico, wizard\*.bmp, dist\varde-agent.exe,
; dist\varde-mesh.exe, dist\varde-tray.exe, dist\varde-control-plane.exe,
; dist\MicrosoftEdgeWebview2Setup.exe
;
; Silent: VardeSetup.exe /VERYSILENT /CPURL=https://cp.example [/TASKS=autostart]
; Silent self-host: /VERYSILENT /TASKS="autostart controlplane"

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
Name: "controlplane"; Description: "Also install the Varde control plane (run the group's server on this PC)"

[Files]
Source: "varde.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\LICENSE"; DestDir: "{app}"
Source: "dist\varde-agent.exe"; DestDir: "{app}"; Flags: ignoreversion; \
  AfterInstall: WriteServerUrl
Source: "dist\varde-mesh.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\varde-tray.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\varde-control-plane.exe"; DestDir: "{app}"; Flags: ignoreversion; \
  Tasks: controlplane
Source: "dist\MicrosoftEdgeWebview2Setup.exe"; DestDir: "{tmp}"; \
  Flags: deleteafterinstall; Check: NeedsWebView2

[Dirs]
Name: "{commonappdata}\Varde\identity"; Permissions: system-full admins-full
Name: "{commonappdata}\Varde"; Permissions: system-full admins-full
; service logs; readable so "Open logs folder" works for any user
Name: "{commonappdata}\Varde\logs"; Permissions: system-full admins-full users-readexec
; control plane data (sqlite db + service logs), only when the task is picked
Name: "{commonappdata}\VardeCP"; Permissions: system-full admins-full; \
  Tasks: controlplane

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
Filename: "{app}\varde-control-plane.exe"; Parameters: "service install"; \
  Tasks: controlplane; Flags: runhidden waituntilterminated; \
  StatusMsg: "Registering the Varde control plane"
Filename: "sc.exe"; Parameters: "start VardeControlPlane"; \
  Tasks: controlplane; Flags: runhidden waituntilterminated; \
  StatusMsg: "Starting the Varde control plane"
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

function IsLocalHttp(U: String): Boolean;
var
  H: String;
  I: Integer;
begin
  H := Lowercase(Copy(U, Length('http://') + 1, MaxInt));
  I := Pos('/', H);
  if I > 0 then
    H := Copy(H, 1, I - 1);
  if (Length(H) > 0) and (H[1] = '[') then
    Result := Pos('[::1]', H) = 1
  else
  begin
    I := Pos(':', H);
    if I > 0 then
      H := Copy(H, 1, I - 1);
    Result := (H = 'localhost') or (Pos('127.', H) = 1);
  end;
end;

// self-hosters pass /CPURL, or pick the controlplane task — that installs a
// local CP, which becomes this PC's default server (overridable later via the
// tray's "Use a different server"). The service reports this address until the
// PC is linked; the tray links with it. Neither present means the hosted
// default, so no server.url is written. A bare host (no scheme) is fine — the
// agent reads it as https.
function EffectiveCpUrl(): String;
begin
  Result := CpUrl();
  if (Result = '') and WizardIsTaskSelected('controlplane') then
    Result := 'http://localhost:8080';
end;

procedure WriteServerUrl();
var
  U: String;
begin
  U := EffectiveCpUrl();
  if IsLinked() or (U = '') then
    Exit;
  if (Pos('http://', Lowercase(U)) = 1) and not IsLocalHttp(U) and
     (MsgBox(U + ' isn''t encrypted (http://). Anyone on the network between this PC and Varde ' +
       'could read or change what it sends.' + #13#10#13#10 +
       'Use it only for a Varde on your own network. Continue?',
       mbConfirmation, MB_YESNO or MB_DEFBUTTON2) <> IDYES) then
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
  // upgrade: stop the tray and remove the old services (waits for Stopped)
  Exec('taskkill.exe', '/F /IM varde-tray.exe', '', SW_HIDE, ewWaitUntilTerminated, Code);
  if FileExists(ExpandConstant('{app}\varde-agent.exe')) then
    Exec(ExpandConstant('{app}\varde-agent.exe'), 'service uninstall', '', SW_HIDE,
      ewWaitUntilTerminated, Code);
  if FileExists(ExpandConstant('{app}\varde-control-plane.exe')) then
    Exec(ExpandConstant('{app}\varde-control-plane.exe'), 'service uninstall', '', SW_HIDE,
      ewWaitUntilTerminated, Code);
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  // upgrade with "Start Varde at login" unticked: drop the earlier Run value
  if (CurStep = ssPostInstall) and not WizardIsTaskSelected('autostart') then
    RegDeleteValue(HKLM, 'Software\Microsoft\Windows\CurrentVersion\Run', 'Varde');
  // upgrade with the control plane unticked: its service was already removed
  // in PrepareToInstall; drop the stale binary too
  if (CurStep = ssPostInstall) and not WizardIsTaskSelected('controlplane') then
    DeleteFile(ExpandConstant('{app}\varde-control-plane.exe'));
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Data: String;
  Code: Integer;
begin
  if CurUninstallStep = usUninstall then
  begin
    // the agent's own UninstallRun entry handles VardeAgent; the control
    // plane service only exists when the component was installed
    if FileExists(ExpandConstant('{app}\varde-control-plane.exe')) then
      Exec(ExpandConstant('{app}\varde-control-plane.exe'), 'service uninstall', '', SW_HIDE,
        ewWaitUntilTerminated, Code);
    Exit;
  end;
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
  Data := ExpandConstant('{commonappdata}\VardeCP');
  if (not UninstallSilent()) and DirExists(Data) and
     (MsgBox('Also delete the control plane''s data?' + #13#10#13#10 +
       'This removes the group''s accounts, servers and save index in ' + Data + '. ' +
       'Save copies on the group''s machines are not affected.',
       mbConfirmation, MB_YESNO or MB_DEFBUTTON2) = IDYES) then
    DelTree(Data, True, True, True);
end;
