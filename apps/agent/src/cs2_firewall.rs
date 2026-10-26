const PREFIX: &str = "Varde CS2 ";

fn rule_name(server: &str) -> Result<String, &'static str> {
    if server.is_empty() || server.len() > 200 || server.chars().any(char::is_control) {
        return Err("cs2: invalid server ID for firewall rule");
    }
    Ok(format!("{PREFIX}{server}"))
}

fn quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "''"))
}

fn sweep_script() -> String {
    format!("$ports=@((Get-NetUDPEndpoint).LocalPort)+@((Get-NetTCPConnection).LocalPort); @($policy.Rules) | Where-Object {{ $_.Name.StartsWith({}, [StringComparison]::Ordinal) }} | ForEach-Object {{ if ($ports -notcontains [int]$_.LocalPorts) {{ $policy.Rules.Remove($_.Name) }} else {{ Write-Warning ('Retaining CS2 rule for a port still in use: '+$_.Name) }} }}", quote(PREFIX))
}

fn cleanup_script(name: &str, owner: &str) -> String {
    format!("@($policy.Rules) | Where-Object {{ $_.Name -ceq {} -and $_.Description -ceq {} }} | ForEach-Object {{ $cleanup={}+$_.Protocol; $_.Name=$cleanup; $policy.Rules.Remove($cleanup) }}", quote(name), quote(owner), quote(&format!("{PREFIX}cleanup {owner} ")))
}

fn install_script(name: &str, owner: &str, port: u16) -> Result<String, &'static str> {
    if port == 0 {
        return Err("cs2: firewall port must be nonzero");
    }
    Ok(format!(
        "foreach ($p in @(1,2,4)) {{ if (!$policy.FirewallEnabled($p)) {{ throw 'Windows Firewall profile is disabled; refusing to expose CS2 ports' }} }}; foreach ($protocol in @(6,17)) {{ $rule=New-Object -ComObject HNetCfg.FWRule; $rule.Name={}; $rule.Description={}; $rule.Protocol=$protocol; $rule.LocalPorts='{port}'; $rule.Direction=1; $rule.Action=0; $rule.Profiles=2147483647; $rule.Enabled=$true; $policy.Rules.Add($rule) }}", quote(name), quote(owner)))
}

#[cfg(windows)]
mod platform {
    use super::*;
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

    pub async fn sweep() -> Result<()> {
        tokio::task::spawn_blocking(|| command(&sweep_script())).await?
    }

    pub struct Rule {
        name: String,
        owner: String,
    }

    impl Drop for Rule {
        fn drop(&mut self) {
            if let Err(error) = command(&cleanup_script(&self.name, &self.owner)) {
                tracing::warn!(%error, rule = %self.name, "CS2 firewall cleanup failed; retaining block until startup sweep");
            }
        }
    }

    pub async fn install(server: &str, port: u16) -> Result<Rule> {
        let name = rule_name(server)?.to_string();
        tokio::task::spawn_blocking(move || {
            let owner = format!("Varde CS2 launch {:032x}", rand::random::<u128>());
            let script = install_script(&name, &owner, port)?;
            let rule = Rule { name, owner };
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

    #[test]
    fn names_and_cleanup_are_server_scoped() {
        assert_eq!(rule_name("a").unwrap(), "Varde CS2 a");
        assert_ne!(rule_name("a").unwrap(), rule_name("b").unwrap());
        assert!(rule_name("").is_err());
        assert!(rule_name("a\nb").is_err());
        assert_eq!(quote("a'b"), "'a''b'");
        let sweep = sweep_script();
        assert!(sweep.contains("StartsWith('Varde CS2 ', [StringComparison]::Ordinal)"));
        assert!(sweep.contains("$policy.Rules.Remove($_.Name)"));
        assert!(sweep.contains("$ports -notcontains [int]$_.LocalPorts"));
    }

    #[test]
    fn inbound_rules_cover_only_the_allocated_port_on_both_protocols() {
        let script = install_script(&rule_name("server-a").unwrap(), "owner-a", 34567).unwrap();
        for value in [
            "$rule.Name='Varde CS2 server-a'",
            "@(6,17)",
            "$rule.LocalPorts='34567'",
            "$rule.Direction=1",
            "$rule.Action=0",
            "$rule.Profiles=2147483647",
            "$policy.FirewallEnabled($p)",
            "$rule.Description='owner-a'",
        ] {
            assert!(script.contains(value), "missing {value}");
        }
        assert!(!script.contains("ApplicationName"));
        assert!(install_script("Varde CS2 server-a", "owner-a", 0).is_err());
        assert_ne!(
            script,
            install_script("Varde CS2 server-b", "owner-b", 34568).unwrap()
        );
    }

    #[test]
    fn cleanup_never_removes_another_launchs_rule_with_the_same_server_name() {
        let first = cleanup_script("Varde CS2 same-server", "owner-a");
        let second = cleanup_script("Varde CS2 same-server", "owner-b");
        assert_ne!(first, second);
        assert!(first
            .contains("$_.Name -ceq 'Varde CS2 same-server' -and $_.Description -ceq 'owner-a'"));
        assert!(first.contains("$_.Name=$cleanup; $policy.Rules.Remove($cleanup)"));
        assert!(!first.contains("Remove('Varde CS2 same-server')"));
    }
}
