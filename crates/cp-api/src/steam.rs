use url::Url;

#[derive(Debug, PartialEq, Eq)]
pub enum SteamNav {
    Allow,
    Finish { query: String },
    External,
}

pub fn steam_login_url_allowed(value: &str) -> bool {
    let Ok(url) = Url::parse(value) else {
        return false;
    };

    url.scheme() == "https"
        && url.host_str() == Some("steamcommunity.com")
        && url.path().starts_with("/openid/login")
}

pub fn steam_callback_origin(redirect_url: &str, cp_base: &str) -> Option<String> {
    let redirect = Url::parse(redirect_url).ok()?;
    let return_to = redirect
        .query_pairs()
        .find(|(name, _)| name == "openid.return_to")
        .map(|(_, value)| value.into_owned())?;
    let callback = Url::parse(&return_to).ok()?;
    if !matches!(callback.scheme(), "http" | "https")
        || callback.path() != "/v1/auth/steam/callback"
        || callback.host_str().is_none()
    {
        return None;
    }
    let authority = callback
        .as_str()
        .split_once("://")?
        .1
        .split(&['/', '?', '#'][..])
        .next()?;
    if authority.contains('@') || !callback.username().is_empty() || callback.password().is_some() {
        return None;
    }

    let cp = Url::parse(cp_base).ok()?;
    if !matches!(cp.scheme(), "http" | "https")
        || (callback.scheme() == "http" && cp.scheme() != "http")
    {
        return None;
    }

    Some(callback.origin().ascii_serialization())
}

pub fn steam_navigation(value: &str, cp_base: &str) -> SteamNav {
    let Ok(url) = Url::parse(value) else {
        return SteamNav::External;
    };

    let host = url.host_str().unwrap_or_default();
    if url.scheme() == "https"
        && (host == "steamcommunity.com"
            || host.ends_with(".steamcommunity.com")
            || host == "steampowered.com"
            || host.ends_with(".steampowered.com"))
    {
        return SteamNav::Allow;
    }

    let Ok(cp) = Url::parse(cp_base) else {
        return SteamNav::External;
    };
    if url.origin() != cp.origin() {
        return SteamNav::External;
    }

    match url.path() {
        "/v1/auth/steam/callback" => SteamNav::Allow,
        "/settings" => SteamNav::Finish {
            query: url.query().unwrap_or_default().to_string(),
        },
        _ => SteamNav::External,
    }
}

#[cfg(test)]
mod tests {
    use super::{steam_callback_origin, steam_login_url_allowed, steam_navigation, SteamNav};
    use url::Url;

    fn redirect_url(return_to: Option<&str>) -> String {
        let mut url = Url::parse("https://steamcommunity.com/openid/login").unwrap();
        if let Some(return_to) = return_to {
            url.query_pairs_mut()
                .append_pair("openid.return_to", return_to);
        }
        url.into()
    }

    #[test]
    fn steam_callback_origin_uses_return_to_host_instead_of_cp_host() {
        let redirect = redirect_url(Some(
            "https://public.example/v1/auth/steam/callback?state=abc",
        ));
        assert_eq!(
            steam_callback_origin(&redirect, "http://10.0.0.5:8080"),
            Some("https://public.example".into())
        );
    }

    #[test]
    fn steam_callback_origin_only_allows_http_for_http_cp() {
        let redirect = redirect_url(Some(
            "http://public.example/v1/auth/steam/callback?state=abc",
        ));
        assert_eq!(steam_callback_origin(&redirect, "https://cp.example"), None);
        assert_eq!(
            steam_callback_origin(&redirect, "http://10.0.0.5:8080"),
            Some("http://public.example".into())
        );
    }

    #[test]
    fn steam_callback_origin_rejects_wrong_path_and_missing_return_to() {
        let wrong_path = redirect_url(Some("https://public.example/other"));
        let missing = redirect_url(None);
        assert_eq!(
            steam_callback_origin(&wrong_path, "https://cp.example"),
            None
        );
        assert_eq!(steam_callback_origin(&missing, "https://cp.example"), None);
    }

    #[test]
    fn steam_callback_origin_rejects_userinfo() {
        let redirect = redirect_url(Some("https://user@public.example/v1/auth/steam/callback"));
        assert_eq!(steam_callback_origin(&redirect, "https://cp.example"), None);
    }

    #[test]
    fn steam_callback_origin_preserves_non_default_port() {
        let redirect = redirect_url(Some("https://public.example:8443/v1/auth/steam/callback"));
        assert_eq!(
            steam_callback_origin(&redirect, "https://cp.example"),
            Some("https://public.example:8443".into())
        );
    }

    #[test]
    fn steam_login_urls_require_https_exact_host_and_openid_path() {
        for url in [
            "https://example.com/openid/login",
            "https://steamcommunity.com.evil.test/openid/login",
            "https://evilsteamcommunity.com/openid/login",
            "http://steamcommunity.com/openid/login",
            "https://steamcommunity.com/other",
        ] {
            assert!(!steam_login_url_allowed(url), "{url}");
        }
        assert!(steam_login_url_allowed(
            "https://steamcommunity.com/openid/login?openid.mode=checkid_setup"
        ));
    }

    #[test]
    fn steam_navigation_allows_steam_hosts_and_subdomains() {
        for url in [
            "https://steamcommunity.com/openid/login",
            "https://login.steamcommunity.com/openid/login",
            "https://steampowered.com/",
            "https://store.steampowered.com/",
        ] {
            assert_eq!(steam_navigation(url, "https://cp.example"), SteamNav::Allow);
        }
    }

    #[test]
    fn steam_navigation_allows_http_cp_callback_and_finishes_settings() {
        let cp = "http://cp.example:8080";
        assert_eq!(
            steam_navigation(
                "http://cp.example:8080/v1/auth/steam/callback?state=abc",
                cp
            ),
            SteamNav::Allow
        );
        assert_eq!(
            steam_navigation("http://cp.example:8080/settings?steam=linked", cp),
            SteamNav::Finish {
                query: "steam=linked".into()
            }
        );
        assert_eq!(
            steam_navigation("http://cp.example:8080/settings?steam_error=taken", cp),
            SteamNav::Finish {
                query: "steam_error=taken".into()
            }
        );
    }

    #[test]
    fn steam_navigation_externalizes_other_paths_and_origins() {
        let cp = "https://cp.example";
        assert_eq!(
            steam_navigation("https://cp.example/other", cp),
            SteamNav::External
        );
        assert_eq!(
            steam_navigation("https://cp.example:8443/v1/auth/steam/callback", cp),
            SteamNav::External
        );
    }
}
