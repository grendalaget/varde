; Varde — NSIS installer hooks, included by installer-hooks/installer.nsi.
; Carries the lifecycle logic the Inno installer (packaging/windows/varde-agent.iss)
; had: Inno→NSIS migration, service drain + register, ProgramData ACLs, the
; opt-in controlplane component, varde:// keys, HKLM autostart, /CPURL silent
; param, remembered component selections, and uninstall data prompts.

Var /GLOBAL CpUrl
Var /GLOBAL TrayArgs

${StrStr}

!define INNO_UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\Varde Agent_is1"
!define VARDE_COMPONENTS_KEY "Software\Varde"
!define RUNKEY "Software\Microsoft\Windows\CurrentVersion\Run"

; ---- shared helpers -------------------------------------------------------

; Apply remembered selections + /TASKS= overrides. Called from the components
; page PRE function (GUI defaults) and again in PREINSTALL (silent installs
; never see the page). Idempotent.
Function VardeApplyTaskParams
  ; /TASKS="autostart controlplane" — when present it is the whole set
  ; (Inno semantics: unlisted tasks are off).
  ${GetOptions} $CMDLINE "/TASKS=" $0
  ${IfNot} ${Errors}
    StrCpy $1 $0
    ${StrStr} $2 $1 "controlplane"
    ${If} $2 == ""
      SectionSetFlags ${sec_controlplane} 0
    ${Else}
      SectionSetFlags ${sec_controlplane} ${SF_SELECTED}
    ${EndIf}
    ${StrStr} $2 $1 "autostart"
    ${If} $2 == ""
      SectionSetFlags ${sec_autostart} 0
    ${Else}
      SectionSetFlags ${sec_autostart} ${SF_SELECTED}
    ${EndIf}
    Return
  ${EndIf}
  ; remembered selections from the last NSIS install (Inno had no
  ; UsePreviousTasks equivalent to carry over; we keep our own marker).
  ; present-but-empty means "user unticked everything" — a missing value is
  ; "no saved selection" (defaults apply); absent and empty differ.
  ClearErrors
  ReadRegStr $0 HKLM "${VARDE_COMPONENTS_KEY}" "Components"
  ${IfNot} ${Errors}
    ${StrStr} $2 $0 "controlplane"
    ${If} $2 == ""
      SectionSetFlags ${sec_controlplane} 0
    ${Else}
      SectionSetFlags ${sec_controlplane} ${SF_SELECTED}
    ${EndIf}
    ${StrStr} $2 $0 "autostart"
    ${If} $2 == ""
      SectionSetFlags ${sec_autostart} 0
    ${Else}
      SectionSetFlags ${sec_autostart} ${SF_SELECTED}
    ${EndIf}
  ${EndIf}
FunctionEnd

; Components-page PRE: show the effective defaults before the user sees them.
Function VardeComponentsPre
  Call VardeApplyTaskParams
  Call SkipIfPassive
FunctionEnd

; Parse /CPURL once (PREINSTALL): /CPURL=https://cp.example, trailing / trimmed.
; Parsed manually — GetOptions treats '/' inside an unquoted scheme:// URL as a
; new switch and truncates the value to "https:". Value runs to EOL or the
; first space (Inno's param semantics); a quoted value honors its quotes.
Function VardeParseCpUrl
  StrCpy $R1 $CMDLINE
  Call VardeParseCpUrlFrom
FunctionEnd

; Parse /CPURL= out of the command line in $R1 → $CpUrl (extracted for testing).
Function VardeParseCpUrlFrom
  StrCpy $CpUrl ""
  ${StrCase} $R0 $R1 "L"                ; case-insensitive match, like Inno params
  ${StrStr} $0 $R0 "/cpurl="
  ${If} $0 != ""
    StrLen $1 $R1
    StrLen $2 $0
    IntOp $1 $1 - $2                    ; index of "/cpurl=" in $R1
    IntOp $1 $1 + 7
    StrCpy $CpUrl $R1 "" $1             ; everything after "/CPURL="
    StrCpy $0 $CpUrl 1
    ${If} $0 == '"'
      StrCpy $CpUrl $CpUrl "" 1         ; drop opening quote
      ${StrStr} $0 $CpUrl '"'           ; $0 = closing quote + tail
      ${If} $0 != ""
        StrLen $1 $CpUrl
        StrLen $2 $0
        IntOp $1 $1 - $2                ; chars inside the quotes
        StrCpy $CpUrl $CpUrl $1
      ${EndIf}
    ${Else}
      ; unquoted: value ends at the first whitespace char or EOL
      StrLen $1 $CpUrl
      StrCpy $2 0
      ${Do}
        ${If} $2 >= $1
          ${ExitDo}
        ${EndIf}
        StrCpy $0 $CpUrl 1 $2
        ${If} $0 == " "
        ${OrIf} $0 == "$\t"
        ${OrIf} $0 == "$\r"
        ${OrIf} $0 == "$\n"
          StrCpy $CpUrl $CpUrl $2
          ${ExitDo}
        ${EndIf}
        IntOp $2 $2 + 1
      ${Loop}
    ${EndIf}
    ${Do}
      StrCpy $0 $CpUrl 1 -1
      ${If} $0 == "/"
        StrCpy $CpUrl $CpUrl -1
      ${Else}
        ${ExitDo}
      ${EndIf}
    ${Loop}
  ${EndIf}
FunctionEnd

; http:// + non-local host → warn unless silent (Inno: silent defaulted to no).
; Returns result in $9: "1" ok to write, "0" skip.
Function VardeCheckPlainHttp
  StrCpy $9 "1"
  StrCpy $0 $CpUrl 7
  ${StrCase} $0 $0 "L"
  ${If} $0 != "http://"
    Return
  ${EndIf}
  ; local hosts, hostname-boundary checked (localhost.example must warn):
  ; localhost[:port|/path|eos], 127.*, [::1][:port|/path|eos]
  StrCpy $0 $CpUrl
  ${StrCase} $0 $0 "L"
  ${If} $0 == "http://localhost"
  ${OrIf} $0 == "http://[::1]"
    Return
  ${EndIf}
  StrCpy $1 $0 17
  ${If} $1 == "http://localhost:"
  ${OrIf} $1 == "http://localhost/"
    Return
  ${EndIf}
  StrCpy $1 $0 11
  ${If} $1 == "http://127."
    Return
  ${EndIf}
  StrCpy $1 $0 13
  ${If} $1 == "http://[::1]:"
  ${OrIf} $1 == "http://[::1]/"
    Return
  ${EndIf}
  ${If} ${Silent}
    ; Inno /VERYSILENT suppressed the box and took the default (No)
    StrCpy $9 "0"
    Return
  ${EndIf}
  MessageBox MB_ICONQUESTION|MB_YESNO|MB_DEFBUTTON2 \
    "$CpUrl isn't encrypted (http://). Anyone on the network between this PC and Varde could read or change what it sends.$\r$\n$\r$\nUse it only for a Varde on your own network. Continue?" \
    /SD IDNO IDYES +2
  StrCpy $9 "0"
FunctionEnd

; icacls: grant SYSTEM + Administrators full (matching Inno's
; Permissions: system-full admins-full); logs also get Users read/exec.
Function VardeAcl
  ; $0 = dir, $1 = extra grant or ""
  ${If} $1 == ""
    nsExec::ExecToStack 'icacls "$0" /inheritance:r /grant "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"'
  ${Else}
    nsExec::ExecToStack 'icacls "$0" /inheritance:r /grant "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" "$1"'
  ${EndIf}
  Pop $2
  Pop $3
FunctionEnd

Function VardeWriteServerUrl
  ; already linked? never overwrite the machine's own server choice
  ${If} ${FileExists} "$APPDATA\Varde\config.toml"
    Return
  ${EndIf}
  StrCpy $0 $CpUrl
  ${If} $0 == ""
    SectionGetFlags ${sec_controlplane} $1
    IntOp $1 $1 & ${SF_SELECTED}
    ${If} $1 <> 0
      StrCpy $0 "http://localhost:8080"
    ${EndIf}
  ${EndIf}
  ${If} $0 == ""
    Return
  ${EndIf}
  StrCpy $CpUrl $0
  Call VardeCheckPlainHttp
  ${If} $9 == "0"
    Return
  ${EndIf}
  FileOpen $1 "$APPDATA\Varde\server.url" w
  FileWrite $1 "$CpUrl$\r$\n"
  FileClose $1
FunctionEnd

; ---- install hooks --------------------------------------------------------

!macro NSIS_HOOK_PREINSTALL
  ; Inno → NSIS migration: an Inno install isn't visible to this installer's
  ; upgrade detection, so uninstall it silently first. Its /VERYSILENT path
  ; drains services (taskkill + service uninstall, waits for Stopped) and
  ; keeps %ProgramData%\Varde (saves + identity) by skipping data prompts.
  ReadRegStr $0 HKLM "${INNO_UNINSTKEY}" "UninstallString"
  ${If} $0 != ""
    DetailPrint "Removing the previous (Inno) Varde install"
    ExecWait '$0 /VERYSILENT /SUPPRESSMSGBOXES /NORESTART' $1
  ${EndIf}

  ; Upgrade drain (same as Inno PrepareToInstall): stop the tray, then each
  ; service uninstalls itself and waits for Stopped (final save + replication).
  nsExec::ExecToStack 'taskkill.exe /F /IM varde-tray.exe'
  Pop $0
  Pop $0
  ${If} ${FileExists} "$INSTDIR\varde-agent.exe"
    DetailPrint "Stopping the Varde service"
    nsExec::ExecToStack '"$INSTDIR\varde-agent.exe" service uninstall'
    Pop $0
    Pop $0
  ${EndIf}
  ${If} ${FileExists} "$INSTDIR\varde-control-plane.exe"
    DetailPrint "Stopping the Varde control plane"
    nsExec::ExecToStack '"$INSTDIR\varde-control-plane.exe" service uninstall'
    Pop $0
    Pop $0
  ${EndIf}

  Call VardeParseCpUrl
  ; interactive installs already folded /TASKS + remembered selections into
  ; the components page (VardeComponentsPre); silent installs never saw it.
  ${If} ${Silent}
    Call VardeApplyTaskParams
  ${EndIf}
!macroend

!macro NSIS_HOOK_POSTINSTALL
  SetShellVarContext all ; $APPDATA = %ProgramData%

  ; data dirs + ACLs (Inno [Dirs])
  CreateDirectory "$APPDATA\Varde"
  StrCpy $0 "$APPDATA\Varde"
  StrCpy $1 ""
  Call VardeAcl
  CreateDirectory "$APPDATA\Varde\identity"
  StrCpy $0 "$APPDATA\Varde\identity"
  StrCpy $1 ""
  Call VardeAcl
  CreateDirectory "$APPDATA\Varde\logs"
  StrCpy $0 "$APPDATA\Varde\logs"
  StrCpy $1 "*S-1-5-32-545:(OI)(CI)RX"
  Call VardeAcl

  Call VardeWriteServerUrl

  ; varde:// hands the varde.games sign-in token to the tray
  WriteRegStr HKLM "Software\Classes\varde" "" "URL:Varde Protocol"
  WriteRegStr HKLM "Software\Classes\varde" "URL Protocol" ""
  WriteRegStr HKLM "Software\Classes\varde\shell\open\command" "" '"$INSTDIR\varde-tray.exe" "%1"'

  ; autostart: all users; the tray can opt out per-user (HKCU)
  SectionGetFlags ${sec_autostart} $0
  IntOp $0 $0 & ${SF_SELECTED}
  ${If} $0 <> 0
    WriteRegStr HKLM "${RUNKEY}" "Varde" '"$INSTDIR\varde-tray.exe" --autostart'
  ${Else}
    ; upgrade with it unticked: drop the earlier value (Inno CurStepChanged)
    DeleteRegValue HKLM "${RUNKEY}" "Varde"
  ${EndIf}

  ; control plane component
  SectionGetFlags ${sec_controlplane} $0
  IntOp $0 $0 & ${SF_SELECTED}
  ${If} $0 <> 0
    CreateDirectory "$APPDATA\VardeCP"
    StrCpy $0 "$APPDATA\VardeCP"
    StrCpy $1 ""
    Call VardeAcl
    DetailPrint "Registering the Varde control plane"
    nsExec::ExecToStack '"$INSTDIR\varde-control-plane.exe" service install'
    Pop $0
    Pop $0
    nsExec::ExecToStack 'sc.exe start VardeControlPlane'
    Pop $0
    Pop $0
  ${Else}
    ; upgrade with it unticked: service already removed above; drop the binary
    Delete "$INSTDIR\varde-control-plane.exe"
  ${EndIf}

  DetailPrint "Registering the Varde service"
  nsExec::ExecToStack '"$INSTDIR\varde-agent.exe" service install'
  Pop $0
  Pop $0
  DetailPrint "Starting the Varde service"
  nsExec::ExecToStack 'sc.exe start VardeAgent'
  Pop $0
  Pop $0

  ; finish page launches the tray into linking on a fresh install
  StrCpy $TrayArgs ""
  ${IfNot} ${FileExists} "$APPDATA\Varde\config.toml"
    StrCpy $TrayArgs "--link"
  ${EndIf}

  ; remember selections for the next install
  StrCpy $0 ""
  SectionGetFlags ${sec_autostart} $1
  IntOp $1 $1 & ${SF_SELECTED}
  ${If} $1 <> 0
    StrCpy $0 "autostart"
  ${EndIf}
  SectionGetFlags ${sec_controlplane} $1
  IntOp $1 $1 & ${SF_SELECTED}
  ${If} $1 <> 0
    StrCpy $0 "$0 controlplane"
  ${EndIf}
  WriteRegStr HKLM "${VARDE_COMPONENTS_KEY}" "Components" "$0"
!macroend

; ---- uninstall hooks ------------------------------------------------------

!macro NSIS_HOOK_PREUNINSTALL
  ; same drain as install (Inno UninstallRun + usUninstall)
  nsExec::ExecToStack 'taskkill.exe /F /IM varde-tray.exe'
  Pop $0
  Pop $0
  ${If} ${FileExists} "$INSTDIR\varde-control-plane.exe"
    nsExec::ExecToStack '"$INSTDIR\varde-control-plane.exe" service uninstall'
    Pop $0
    Pop $0
  ${EndIf}
  ${If} ${FileExists} "$INSTDIR\varde-agent.exe"
    nsExec::ExecToStack '"$INSTDIR\varde-agent.exe" service uninstall'
    Pop $0
    Pop $0
  ${EndIf}
  DeleteRegValue HKLM "${RUNKEY}" "Varde"
  DeleteRegKey HKLM "Software\Classes\varde"
  DeleteRegKey HKLM "${VARDE_COMPONENTS_KEY}"
!macroend

!macro NSIS_HOOK_POSTUNINSTALL
  ; the confirm page's "delete app data" checkbox covers both data dirs
  ; (Inno asked about each; silent uninstalls keep both, as before)
  ${If} $DeleteAppDataCheckboxState = 1
    SetShellVarContext all
    RMDir /r "$APPDATA\Varde"
    RMDir /r "$APPDATA\VardeCP"
  ${EndIf}
!macroend
