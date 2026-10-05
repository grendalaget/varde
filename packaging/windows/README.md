# Windows packaging

`varde-agent.iss` (Inno Setup 6) builds `VardeSetup-<ver>.exe`, which:

- installs `varde-agent.exe` + `varde-mesh.exe` into `Program Files\Varde`
- creates `%ProgramData%\Varde` with a SYSTEM/Administrators-only ACL on
  `identity\`
- registers and starts the `VardeAgent` Windows service (the agent's own
  `service install` path — STOP and PRESHUTDOWN map to the graceful-stop
  path, with a 120 s preshutdown timeout)
- offers to open the enrollment page at the end

```sh
iscc /DVersion=0.1.0 packaging/windows/varde-agent.iss
```

expects `packaging/windows/dist/varde-agent.exe` and
`packaging/windows/dist/varde-mesh.exe`. CI job `windows-installer`
builds both (MSVC toolchain for the agent), runs ISCC via
`choco install innosetup`, and uploads `VardeSetup-*` as an artifact.
