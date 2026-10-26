const PREFIX: &str = "Varde CS2 ";

struct SweepGate(tokio::sync::watch::Receiver<Option<Result<(), String>>>);

impl SweepGate {
    fn start(work: impl std::future::Future<Output = Result<(), String>> + Send + 'static) -> Self {
        let (done, waiting) = tokio::sync::watch::channel(None);
        tokio::spawn(async move {
            done.send_replace(Some(work.await));
        });
        Self(waiting)
    }

    async fn wait(&self) -> Result<(), String> {
        self.0.clone().wait_for(Option::is_some).await
            .map_err(|_| "CS2 firewall sweep task stopped".to_string())?
            .as_ref().unwrap().clone()
    }
}

fn rule_names(server: &str, launch: u128) -> Result<[String; 2], &'static str> {
    if server.is_empty() || server.len() > 160 || server.chars().any(char::is_control) {
        return Err("cs2: invalid server ID for firewall rule");
    }
    Ok(["tcp", "udp"].map(|protocol| format!("{PREFIX}{server} {launch:032x} {protocol}")))
}

fn quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "''"))
}

fn sweep_script() -> String {
    format!("$ports=@((Get-NetUDPEndpoint).LocalPort)+@((Get-NetTCPConnection | Where-Object {{ $_.State -eq 'Listen' }}).LocalPort); @($policy.Rules) | Where-Object {{ $_.Name.StartsWith({}, [StringComparison]::Ordinal) }} | ForEach-Object {{ if ($ports -notcontains [int]$_.LocalPorts) {{ $unique={}+[Guid]::NewGuid().ToString('N'); $_.Name=$unique; $policy.Rules.Remove($unique) }} else {{ Write-Warning ('Retaining CS2 rule for a port still in use: '+$_.Name) }} }}", quote(PREFIX), quote(&format!("{PREFIX}stale ")))
}

fn cleanup_script(names: &[String; 2]) -> String {
    format!("foreach ($name in @({},{})) {{ $policy.Rules.Remove($name) }}", quote(&names[0]), quote(&names[1]))
}

fn install_script(names: &[String; 2], port: u16) -> Result<String, &'static str> {
    if port == 0 { return Err("cs2: firewall port must be nonzero"); }
    let mut script = "foreach ($p in @(1,2,4)) { if (!$policy.FirewallEnabled($p)) { throw 'Windows Firewall profile is disabled; refusing to expose CS2 ports' } }; ".to_string();
    for (name, protocol) in names.iter().zip([6, 17]) {
        script.push_str(&format!("$rule=New-Object -ComObject HNetCfg.FWRule; $rule.Name={}; $rule.Description='Varde private CS2 port'; $rule.Protocol={protocol}; $rule.LocalPorts='{port}'; $rule.Direction=1; $rule.Action=0; $rule.Profiles=2147483647; $rule.Enabled=$true; $policy.Rules.Add($rule); ", quote(name)));
    }
    Ok(script)
}

#[cfg(windows)]
mod platform {
    use super::*;

    #[test]
    fn sweep_ignores_time_wait_and_connected_tcp_sockets() {
        let script = sweep_script();
        assert!(script.contains("Get-NetUDPEndpoint"));
        assert!(script.contains("Get-NetTCPConnection | Where-Object { $_.State -eq 'Listen' }"));
        assert!(!script.contains("(Get-NetTCPConnection).LocalPort"));
    }
    use executor_api::*;
    use std::os::windows::process::CommandExt;
    use std::sync::Arc;

    fn command(script: &str) -> Result<()> {
        let system =
            std::env::var_os("SystemRoot").ok_or("cs2: Windows system directory unavailable")?;
        let powershell =
            std::path::PathBuf::from(system).join("System32/WindowsPowerShell/v1.0/powershell.exe");
        let output = std::process::Command::new(powershell)
            .args(["-NoProfile", "-NonInteractive", "-Command", &format!("$ErrorActionPreference='Stop'; $policy=New-Object -ComObject HNetCfg.FwPolicy2; {script}")])
            .creation_flags(0x08000000)
            .output()?;
        if !output.status.success() {
            return Err(format!(
                "cs2: Windows Firewall COM operation failed: {}",
                String::from_utf8_lossy(&output.stderr)
            )
            .into());
        }
        Ok(())
    }

    fn startup() -> &'static SweepGate {
        static SWEEP: std::sync::OnceLock<SweepGate> = std::sync::OnceLock::new();
        SWEEP.get_or_init(|| SweepGate::start(async {
            async { tokio::task::spawn_blocking(|| command(&sweep_script())).await? }.await
                .map_err(|error: executor_api::DynError| error.to_string())
        }))
    }

    pub fn start_sweep() {
        let _ = startup();
    }

    pub struct Rule {
        names: [String; 2],
    }

    impl Drop for Rule {
        fn drop(&mut self) {
            if let Err(error) = command(&cleanup_script(&self.names)) {
                tracing::warn!(%error, rules = ?self.names, "CS2 firewall cleanup failed; retaining block until startup sweep");
            }
        }
    }

    pub async fn install(server: &str, port: u16) -> Result<Rule> {
        startup().wait().await?;
        let names = rule_names(server, rand::random())?;
        tokio::task::spawn_blocking(move || {
            let script = install_script(&names, port)?;
            let rule = Rule { names };
            if let Err(error) = command(&script) {
                tracing::warn!(%error, "CS2 firewall protection unavailable; launch refused");
                return Err(error);
            }
            Ok(rule)
        })
        .await?
    }

    pub fn retain(process: Box<dyn ProcessHandle>, rule: Rule) -> Box<dyn ProcessHandle> {
        let process: Arc<dyn ProcessHandle> = Arc::from(process);
        let watched = process.clone();
        let (done, cleaned) = tokio::sync::watch::channel(false);
        tokio::spawn(async move {
            if let Err(error) = watched.wait().await {
                tracing::warn!(%error, "CS2 exit unknown; retaining firewall block");
                std::future::pending::<()>().await;
            }
            let _ = tokio::task::spawn_blocking(move || drop(rule)).await;
            done.send_replace(true);
        });
        Box::new(ProtectedProcess { process, cleaned })
    }

    struct ProtectedProcess {
        process: Arc<dyn ProcessHandle>,
        cleaned: tokio::sync::watch::Receiver<bool>,
    }

    #[async_trait::async_trait]
    impl ProcessHandle for ProtectedProcess {
        fn pid(&self) -> u32 {
            self.process.pid()
        }
        async fn write_stdin(&self, line: &str) -> Result<()> {
            self.process.write_stdin(line).await
        }
        fn output(&self) -> tokio::sync::broadcast::Receiver<OutputLine> {
            self.process.output()
        }
        fn output_tail(&self, n: usize) -> Vec<OutputLine> {
            self.process.output_tail(n)
        }
        async fn wait(&self) -> Result<ExitStatus> {
            let status = self.process.wait().await?;
            self.cleaned.clone().wait_for(|done| *done).await?;
            Ok(status)
        }
        async fn terminate(&self) -> Result<()> {
            self.process.terminate().await
        }
        async fn interrupt(&self) -> Result<()> {
            self.process.interrupt().await
        }
        async fn kill(&self) -> Result<()> {
            self.process.kill().await
        }
        fn resource_usage(&self) -> Option<ResourceUsage> {
            self.process.resource_usage()
        }
    }
}

#[cfg(windows)]
pub use platform::*;

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn background_sweep_only_blocks_consumers_and_caches_completion() {
        let (release, work) = tokio::sync::oneshot::channel();
        let gate = SweepGate::start(async { work.await.unwrap() });
        assert!(tokio::time::timeout(std::time::Duration::from_millis(10), gate.wait()).await.is_err());
        release.send(Ok(())).unwrap();
        assert_eq!(gate.wait().await, Ok(()));
        assert_eq!(gate.wait().await, Ok(()));
        let failure = SweepGate::start(async { Err("sweep failed".to_string()) });
        assert_eq!(failure.wait().await, Err("sweep failed".to_string()));
        assert_eq!(failure.wait().await, Err("sweep failed".to_string()));
    }

    #[test]
    fn unique_names_and_exact_cleanup_cover_protocols_launches_and_servers() {
        let names = rule_names("server-a", 1).unwrap();
        assert_ne!(names[0], names[1]);
        for name in &names {
            assert!(name.starts_with(PREFIX));
            assert!(!rule_names("server-a", 2).unwrap().contains(name));
            assert!(!rule_names("server-b", 1).unwrap().contains(name));
        }
        assert!(rule_names("", 0).is_err());
        assert!(rule_names("a\nb", 0).is_err());
        assert_eq!(quote("a'b"), "'a''b'");
        let cleanup = cleanup_script(&names);
        for name in &names { assert!(cleanup.contains(&quote(name))); }
        assert!(cleanup.contains("$policy.Rules.Remove($name)"));
        assert!(!cleanup.contains(".Name="));
        assert_eq!(cleanup_script(&names), cleanup);
    }

    #[test]
    fn sweep_excludes_time_wait_and_disambiguates_legacy_duplicate_names() {
        let sweep = sweep_script();
        assert!(sweep.contains("Get-NetUDPEndpoint"));
        assert!(sweep.contains("Get-NetTCPConnection | Where-Object { $_.State -eq 'Listen' }"));
        assert!(sweep.contains("StartsWith('Varde CS2 ', [StringComparison]::Ordinal)"));
        assert!(sweep.contains("$ports -notcontains [int]$_.LocalPorts"));
        assert!(sweep.contains("$unique='Varde CS2 stale '+[Guid]::NewGuid().ToString('N')"));
        assert!(sweep.contains("$_.Name=$unique; $policy.Rules.Remove($unique)"));
    }

    #[test]
    fn inbound_rules_cover_only_the_allocated_port_on_both_protocols() {
        let names = rule_names("server-a", 1).unwrap();
        let script = install_script(&names, 34567).unwrap();
        for value in ["$rule.Protocol=6", "$rule.Protocol=17", "$rule.LocalPorts='34567'", "$rule.Direction=1", "$rule.Action=0", "$rule.Profiles=2147483647", "$policy.FirewallEnabled($p)"] {
            assert!(script.contains(value), "missing {value}");
        }
        for name in &names { assert!(script.contains(&quote(name))); }
        assert!(!script.contains("ApplicationName"));
        assert!(install_script(&names, 0).is_err());
    }
}
