//! WireGuard and WARP profile authoring.

use adw::prelude::*;
use std::rc::Rc;

use crate::app;
use crate::dialogs;
use crate::models::{human_bytes, WireGuardProfile};
use crate::store::Shared;

/// Applies a WireGuard configuration pasted into the form.
pub fn add_wireguard(state: &Shared, parent: &impl IsA<gtk::Widget>) {
    let form = dialogs::form("Add WireGuard profile", "Apply", 520);
    let list = dialogs::boxed_rows();

    let name = adw::EntryRow::builder().title("Profile name").build();
    let config = gtk::TextView::builder()
        .monospace(true)
        .top_margin(8)
        .bottom_margin(8)
        .left_margin(8)
        .right_margin(8)
        .wrap_mode(gtk::WrapMode::Char)
        .build();
    let config_scroller = gtk::ScrolledWindow::builder()
        .min_content_height(180)
        .hscrollbar_policy(gtk::PolicyType::Never)
        .css_classes(["card"])
        .child(&config)
        .build();
    let config_label = gtk::Label::builder()
        .label("Configuration")
        .xalign(0.0)
        .css_classes(["dim-label"])
        .build();

    list.append(&name);
    form.body.append(&config_label);
    form.body.append(&config_scroller);

    let state = Rc::clone(state);
    form.on_accept(move || {
        let buffer = config.buffer();
        let text = buffer.text(&buffer.start_iter(), &buffer.end_iter(), false);
        if text.trim().is_empty() {
            state.toast("Paste a WireGuard configuration first");
            return false;
        }
        let mut args = vec![
            "wireguard".to_string(),
            "apply".into(),
            "--file".into(),
            "-".into(),
        ];
        let name = name.text().trim().to_string();
        if !name.is_empty() {
            args.push("--name".into());
            args.push(name);
        }
        state.mutate(args, Some(text.to_string()));
        true
    });
    form.present(parent);
}

/// Registers a WARP profile, either fresh or from a Zero Trust token.
pub fn register_warp(state: &Shared, parent: &impl IsA<gtk::Widget>) {
    let form = dialogs::form("Register WARP profile", "Register", 460);
    let list = dialogs::boxed_rows();

    let name = adw::EntryRow::builder().title("Profile name").build();
    let jwt = adw::PasswordEntryRow::builder()
        .title("Zero Trust token (optional)")
        .build();
    let transport = adw::ComboRow::builder()
        .title("Transport")
        .model(&gtk::StringList::new(&["QUIC", "HTTP/2"]))
        .build();
    transport.set_selected(0);
    let dns = adw::SwitchRow::builder()
        .title("Use WARP DNS")
        .subtitle("Route DNS through the WARP profile")
        .build();
    let tos = adw::SwitchRow::builder()
        .title("Accept the WARP terms of service")
        .build();

    list.append(&name);
    list.append(&jwt);
    list.append(&transport);
    list.append(&dns);
    list.append(&tos);
    form.body.append(&list);

    let state = Rc::clone(state);
    form.on_accept(move || {
        let profile = name.text().trim().to_string();
        if profile.is_empty() {
            state.toast("A profile name is required");
            return false;
        }
        if !tos.is_active() {
            state.toast("Accept the terms of service first");
            return false;
        }
        let mut args = vec!["warp".to_string(), "register".into(), profile];
        let token = jwt.text().to_string();
        if !token.trim().is_empty() {
            args.push("--jwt".into());
            args.push("-".into());
        }
        args.push("--transport".into());
        args.push(
            if transport.selected() == 1 {
                "http2"
            } else {
                "quic"
            }
            .into(),
        );
        if dns.is_active() {
            args.push("--dns".into());
        }
        args.push("--accept-tos".into());
        let secret = if token.trim().is_empty() {
            None
        } else {
            Some(token)
        };
        state.mutate(args, secret);
        true
    });
    form.present(parent);
}

/// Shows the applied manifest and its peers for one WireGuard profile.
pub fn detail(state: &Shared, name: &str) -> gtk::Widget {
    let (scroller, content) = app::page_body();

    let summary = app::group("WireGuard");
    let summary_list = app::boxed_list();
    summary.add(&summary_list);
    content.append(&summary);

    let peers = app::group("Peers");
    let peers_list = app::boxed_list();
    peers.add(&peers_list);
    content.append(&peers);

    let shields = adw::SwitchRow::builder()
        .title("Shields up")
        .subtitle("Refuse inbound connections")
        .build();
    summary_list.append(&shields);

    let loading = Rc::new(std::cell::Cell::new(false));
    let refresh = {
        let state = Rc::clone(state);
        let name = name.to_string();
        let summary_list = summary_list.clone();
        let peers_list = peers_list.clone();
        let shields = shields.clone();
        let loading = Rc::clone(&loading);
        move || {
            let bridge = state.bridge.clone();
            let name = name.clone();
            let summary_list = summary_list.clone();
            let peers_list = peers_list.clone();
            let shields = shields.clone();
            let loading = Rc::clone(&loading);
            gtk::glib::spawn_future_local(async move {
                let args = ["wireguard".to_string(), "show".into(), name.clone()];
                let fetched = gtk::gio::spawn_blocking(move || bridge.read_cli(&args)).await;
                app::clear(&peers_list);
                // The summary rows are rebuilt every poll; clear them or they pile up.
                app::clear(&summary_list);
                summary_list.append(&shields);
                let profile = match fetched {
                    Ok(Ok(text)) => match serde_json::from_str::<WireGuardProfile>(&text) {
                        Ok(profile) => profile,
                        Err(_) => {
                            summary_list.append(&app::row(
                                "Unreadable manifest",
                                "The CLI returned unexpected JSON",
                            ));
                            return;
                        }
                    },
                    Ok(Err(failure)) => {
                        summary_list
                            .append(&app::row("Could not read the profile", &failure.message));
                        return;
                    }
                    Err(_) => {
                        summary_list.append(&app::row(
                            "Could not read the profile",
                            "The command did not finish.",
                        ));
                        return;
                    }
                };
                loading.set(true);
                shields.set_active(profile.shields_up);
                loading.set(false);
                summary_list.append(&app::row("Public key", &profile.public_key));
                summary_list.append(&app::row("Listen port", &profile.listen_port.to_string()));
                if !profile.addresses.is_empty() {
                    summary_list.append(&app::row("Addresses", &profile.addresses.join(", ")));
                }
                if !profile.dns_suffix.is_empty() {
                    summary_list.append(&app::row("DNS suffix", &profile.dns_suffix));
                }
                summary_list.append(&app::row("Grants", &profile.grant_count.to_string()));
                if profile.peers.is_empty() {
                    peers_list.append(&app::row("No peers", "The manifest declares no peers"));
                }
                for peer in &profile.peers {
                    let title = if peer.name.is_empty() {
                        peer.endpoint.clone()
                    } else {
                        peer.name.clone()
                    };
                    let subtitle = format!(
                        "{} · ↑ {} · ↓ {}",
                        if peer.online { "online" } else { "offline" },
                        human_bytes(peer.transmit_bytes),
                        human_bytes(peer.receive_bytes)
                    );
                    peers_list.append(&app::row(&title, &subtitle));
                }
            });
        }
    };

    refresh();
    {
        let state = Rc::clone(state);
        let name = name.to_string();
        let loading = Rc::clone(&loading);
        shields.connect_active_notify(move |row| {
            if loading.get() {
                return;
            }
            let value = if row.is_active() { "on" } else { "off" };
            state.mutate(
                vec![
                    "wireguard".into(),
                    "shields-up".into(),
                    name.clone(),
                    value.into(),
                ],
                None,
            );
        });
    }

    let view = adw::ToolbarView::new();
    let header = adw::HeaderBar::new();
    header.set_title_widget(Some(&gtk::Label::new(Some(name))));
    let state = Rc::clone(state);
    state.subscribe(refresh);
    view.add_top_bar(&header);
    view.set_content(Some(&scroller));
    view.upcast()
}
