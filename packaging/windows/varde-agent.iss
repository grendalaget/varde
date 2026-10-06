; Varde — Inno Setup installer.
; Installs the agent service, mesh and tray into Program Files, asks for the
; Varde address, starts the service and opens the tray's link window.
;
;   iscc /DVersion=0.1.0 varde-agent.iss
; Expects alongside this script: varde.ico, dist\varde-agent.exe,
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
UninstallDisplayIcon={app}\varde.ico
CloseApplications=force

[Tasks]
Name: "autostart"; Description: "Start Varde at login (tray icon)"

[Files]
Source: "varde.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\varde-agent.exe"; DestDir: "{app}"; Flags: ignoreversion; \
  AfterInstall: WriteServerUrl
Source: "dist\varde-mesh.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\varde-tray.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\MicrosoftEdgeWebview2Setup.exe"; DestDir: "{tmp}"; \
  Flags: deleteafterinstall; Check: NeedsWebView2

[Dirs]
Name: "{commonappdata}\Varde\identity"; Permissions: system-full admins-full
Name: "{commonappdata}\Varde"; Permissions: system-full admins-full

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

var
  UrlPage: TInputQueryWizardPage;

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
  Result := Trim(UrlPage.Values[0]);
  while (Length(Result) > 0) and (Result[Length(Result)] = '/') do
    Delete(Result, Length(Result), 1);
end;

// the service reports this address until the PC is linked; the tray links with it
procedure WriteServerUrl();
begin
  if not IsLinked() then
    SaveStringToFile(ExpandConstant('{commonappdata}\Varde\server.url'), CpUrl() + #13#10, False);
end;

function TrayArgs(Param: String): String;
begin
  if IsLinked() then
    Result := ''
  else
    Result := '--link';
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

function TrayRunLabel(Param: String): String;
begin
  if IsLinked() then
    Result := 'Start the Varde tray'
  else
    Result := 'Link this PC now';
end;

procedure InitializeWizard();
begin
  UrlPage := CreateInputQueryPage(wpSelectTasks,
    'Varde address', 'Which Varde should this PC join?',
    'Enter the address of your Varde dashboard. After installing, Varde shows a short code ' +
    'and opens this address so you can approve the PC.');
  UrlPage.Add('Varde address:', False);
  UrlPage.Values[0] := ExpandConstant('{param:CPURL|https://varde.games}');
end;

function ShouldSkipPage(PageID: Integer): Boolean;
begin
  Result := (PageID = UrlPage.ID) and IsLinked();
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  U: String;
begin
  Result := True;
  if CurPageID <> UrlPage.ID then
    Exit;
  U := CpUrl();
  if (Pos('https://', Lowercase(U)) <> 1) and (Pos('http://', Lowercase(U)) <> 1) then
  begin
    MsgBox('Enter an address starting with https:// (or http:// for a local Varde).',
      mbError, MB_OK);
    Result := False;
    Exit;
  end;
  if (Pos('http://', Lowercase(U)) = 1) and not IsLocalHttp(U) and
     (MsgBox(U + ' isn''t encrypted (http://). Anyone on the network between this PC and Varde ' +
       'could read or change what it sends.' + #13#10#13#10 +
       'Use it only for a Varde on your own network. Continue?',
       mbConfirmation, MB_YESNO or MB_DEFBUTTON2) <> IDYES) then
  begin
    Result := False;
    Exit;
  end;
  try
    DownloadTemporaryFile(U + '/v1/version', 'varde-version.json', '', nil);
  except
    Result := MsgBox('Couldn''t reach ' + U + ' right now.' + #13#10#13#10 +
      'Continue anyway? You can link this PC later from the tray.',
      mbConfirmation, MB_YESNO) = IDYES;
  end;
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
