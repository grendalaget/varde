; Varde Agent — Inno Setup installer.
; Builds VardeSetup.exe which installs the agent + mesh into Program Files,
; registers and starts the Windows service, and opens enrollment at the end.
;
;   iscc /DVersion=0.1.0 varde-agent.iss
; Expects alongside this script: varde.ico, dist\varde-agent.exe,
; dist\varde-mesh.exe

#ifndef Version
  #define Version "0.0.0-dev"
#endif

[Setup]
AppName=Varde Agent
AppVersion={#Version}
AppPublisher=Varde contributors
DefaultDirName={autopf}\Varde
DefaultGroupName=Varde
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

[Files]
Source: "varde.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\varde-agent.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\varde-mesh.exe"; DestDir: "{app}"; Flags: ignoreversion

[Dirs]
Name: "{commonappdata}\Varde\identity"; Permissions: system-full admins-full
Name: "{commonappdata}\Varde"; Permissions: system-full admins-full

[Run]
Filename: "{app}\varde-agent.exe"; Parameters: "service install"; \
  Flags: runhidden waituntilterminated; \
  StatusMsg: "Registering the Varde Agent service"
Filename: "sc.exe"; Parameters: "start VardeAgent"; \
  Flags: runhidden waituntilterminated; \
  StatusMsg: "Starting the Varde Agent service"
Filename: "https://varde.games/enroll"; \
  Flags: postinstall shellexec unchecked; \
  Description: "Open the enrollment page"

[UninstallRun]
Filename: "{app}\varde-agent.exe"; Parameters: "service uninstall"; \
  Flags: runhidden waituntilterminated; RunOnceId: "svcuninstall"

[Code]
function InitializeSetup(): Boolean;
begin
  Result := True;
end;
