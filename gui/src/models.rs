//! The subset of the daemon's JSON types the GUI renders.
//!
//! Field names mirror `controlapi/types.go` exactly. Missing or extra fields
//! are tolerated so a newer daemon does not break an older GUI.

use serde::Deserialize;

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Status {
    pub state: String,
    pub profiles: Vec<Profile>,
    pub ip_routes: IpRoutes,
    pub exit_nodes: ExitNodes,
    pub dns_routes: DnsRoutes,
    pub search_domains: SearchDomains,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Profile {
    pub id: String,
    pub name: String,
    pub kind: String,
    pub state_dir: String,
    pub hostname: String,
    pub enabled: bool,
    pub removed: bool,
    pub accept_all_routes: bool,
    pub accept_all_dns_routes: bool,
    pub runtime_state: String,
    pub backend_state: String,
    pub default_route: bool,
    pub last_connected: String,
    pub magic_dns_suffix: String,
    pub self_dns_name: String,
    pub self_ips: Vec<String>,
    pub peer_count: i64,
    pub shields_up: bool,
    pub auth_url: String,
    pub local_api_socket: String,
    pub last_error: String,
}

impl Profile {
    /// Short kind label: `tailscale`, `wireguard` or `warp`.
    pub fn kind_label(&self) -> &str {
        if self.kind.is_empty() {
            "tailscale"
        } else {
            &self.kind
        }
    }

    /// Human state used in list rows and the header pill.
    pub fn state_label(&self) -> String {
        if self.removed {
            return "Removed".to_string();
        }
        if !self.enabled {
            return "Disabled".to_string();
        }
        if !self.last_error.is_empty() {
            return "Degraded".to_string();
        }
        match self.runtime_state.as_str() {
            "running" => "Connected".to_string(),
            "starting" => "Starting".to_string(),
            "stopping" => "Stopping".to_string(),
            "stopped" => "Stopped".to_string(),
            "" => "Unknown".to_string(),
            other => capitalize(other),
        }
    }

    pub fn is_connected(&self) -> bool {
        self.enabled && !self.removed && self.runtime_state == "running"
    }
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct IpRoutes {
    pub accept_all_profiles: Vec<AcceptAllProfile>,
    pub bindings: Vec<IpRouteBinding>,
    pub imported: Vec<IpRouteBinding>,
    pub available: Vec<AvailableIpRoute>,
    pub reconcile_error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct AcceptAllProfile {
    pub profile_id: String,
    pub profile_name: String,
    pub state: String,
    pub reason: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct IpRouteBinding {
    pub prefix: String,
    pub profile_id: String,
    pub profile_name: String,
    pub policy: String,
    pub state: String,
    pub reason: String,
    pub covering_route: String,
    pub primary_router: String,
    pub overridden_by: String,
    pub override_profile_name: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct AvailableIpRoute {
    pub prefix: String,
    pub profile_id: String,
    pub profile_name: String,
    pub primary_router: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct ExitNodes {
    pub selected: Option<SelectedExitNode>,
    pub available: Vec<AvailableExitNode>,
    pub reconcile_error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct SelectedExitNode {
    pub profile_id: String,
    pub profile_name: String,
    pub node_id: String,
    pub dns_name: String,
    pub peer_ip: String,
    pub online: bool,
    pub location: Option<ExitNodeLocation>,
    pub state: String,
    pub reason: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct AvailableExitNode {
    pub profile_id: String,
    pub profile_name: String,
    pub node_id: String,
    pub dns_name: String,
    pub ips: Vec<String>,
    pub online: bool,
    pub location: Option<ExitNodeLocation>,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct ExitNodeLocation {
    pub country: String,
    pub country_code: String,
    pub city: String,
    pub city_code: String,
    pub priority: i64,
}

impl ExitNodeLocation {
    pub fn label(&self) -> String {
        match (self.city.is_empty(), self.country.is_empty()) {
            (false, false) => format!("{}, {}", self.city, self.country),
            (false, true) => self.city.clone(),
            (true, false) => self.country.clone(),
            (true, true) => String::new(),
        }
    }
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct DnsRoutes {
    pub accept_all_profiles: Vec<AcceptAllProfile>,
    pub bindings: Vec<DnsRouteBinding>,
    pub imported: Vec<DnsRouteBinding>,
    pub automatic: Vec<DnsRouteBinding>,
    pub available: Vec<AvailableDnsRoute>,
    pub reconcile_error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct DnsRouteBinding {
    pub domain: String,
    pub profile_id: String,
    pub profile_name: String,
    pub policy: String,
    pub source: String,
    pub state: String,
    pub reason: String,
    pub covering_route: String,
    pub resolvers: Vec<DnsResolver>,
    pub overridden_by: String,
    pub override_profile_name: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct AvailableDnsRoute {
    pub domain: String,
    pub profile_id: String,
    pub profile_name: String,
    pub source: String,
    pub resolvers: Vec<DnsResolver>,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct DnsResolver {
    pub addr: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct SearchDomains {
    pub desired: Vec<String>,
    pub installed: Vec<InstalledSearchDomain>,
    pub waiting: Vec<WaitingSearchDomain>,
    pub available: Vec<AvailableSearchDomain>,
    pub reconcile_error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct InstalledSearchDomain {
    pub domain: String,
    pub profile_id: String,
    pub profile_name: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct WaitingSearchDomain {
    pub domain: String,
    pub reason: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct AvailableSearchDomain {
    pub domain: String,
    pub profile_id: String,
    pub profile_name: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct UpdateStatus {
    pub enabled: bool,
    pub current_version: String,
    pub available_version: String,
    pub state: String,
    pub last_checked: String,
    pub last_error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct WireGuardProfile {
    pub name: String,
    pub kind: String,
    pub public_key: String,
    pub listen_port: u16,
    pub addresses: Vec<String>,
    pub dns_suffix: String,
    pub shields_up: bool,
    pub grant_count: i64,
    pub peers: Vec<WireGuardPeer>,
}

#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct WireGuardPeer {
    pub name: String,
    pub public_key: String,
    pub endpoint: String,
    pub online: bool,
    pub last_handshake: String,
    pub receive_bytes: i64,
    pub transmit_bytes: i64,
    pub canonical_addresses: Vec<String>,
    pub effective_addresses: Vec<String>,
    pub routes: Vec<String>,
    pub exit_node: bool,
    pub exit_node_selected: bool,
}

fn capitalize(value: &str) -> String {
    let mut chars = value.chars();
    match chars.next() {
        Some(first) => first.to_uppercase().collect::<String>() + chars.as_str(),
        None => String::new(),
    }
}

/// Formats a byte count the way the CLI's peer listings do.
pub fn human_bytes(bytes: i64) -> String {
    const UNITS: [&str; 5] = ["B", "KiB", "MiB", "GiB", "TiB"];
    let mut value = bytes as f64;
    let mut unit = 0;
    while value >= 1024.0 && unit < UNITS.len() - 1 {
        value /= 1024.0;
        unit += 1;
    }
    if unit == 0 {
        format!("{} {}", bytes, UNITS[unit])
    } else {
        format!("{value:.1} {}", UNITS[unit])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn formats_byte_counts() {
        assert_eq!(human_bytes(0), "0 B");
        assert_eq!(human_bytes(1023), "1023 B");
        assert_eq!(human_bytes(1024), "1.0 KiB");
        assert_eq!(human_bytes(5 * 1024 * 1024), "5.0 MiB");
    }

    #[test]
    fn labels_a_location_with_city_and_country() {
        let full = ExitNodeLocation {
            city: "Berlin".into(),
            country: "Germany".into(),
            ..Default::default()
        };
        assert_eq!(full.label(), "Berlin, Germany");
        let country_only = ExitNodeLocation {
            country: "Germany".into(),
            ..Default::default()
        };
        assert_eq!(country_only.label(), "Germany");
        assert_eq!(ExitNodeLocation::default().label(), "");
    }

    #[test]
    fn decodes_a_profile_with_missing_fields() {
        let profile: Profile = serde_json::from_str(
            "{\"name\":\"work\",\"enabled\":true,\"runtimeState\":\"running\"}",
        )
        .unwrap();
        assert_eq!(profile.name, "work");
        assert_eq!(profile.state_label(), "Connected");
        assert!(profile.is_connected());
        assert_eq!(profile.peer_count, 0);
    }
}
