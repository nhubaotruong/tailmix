//! Profiles view: the list of tailnets and one detail page per profile.

use adw::prelude::*;
use std::rc::Rc;

use crate::app::{self, Shell};
use crate::dialogs;
use crate::models::Profile;
use crate::store::Shared;

pub fn build(state: &Shared, shell: &Rc<Shell>) -> gtk::Widget {
    let (scroller, content) = app::page_body();
    let group = app::group("Profiles");
    content.append(&group);
    let list = app::boxed_list();
    group.add(&list);

    let refresh = {
        let state = Rc::clone(state);
        let shell = Rc::clone(shell);
        let list = list.clone();
        move || {
            app::clear(&list);
            let core = state.core();
            match core.status.as_ref() {
                None => list.append(&app::row("tailmixd is not running", "")),
                Some(status) if status.profiles.is_empty() => {
                    list.append(&app::row("No profiles yet", "Add a tailnet to get started"))
                }
                Some(status) => {
                    for profile in status.profiles.iter().filter(|profile| !profile.removed) {
                        let row = app::row(&profile.name, &subtitle(profile));
                        row.add_suffix(&gtk::Image::from_icon_name("go-next-symbolic"));
                        let state = Rc::clone(&state);
                        let shell = Rc::clone(&shell);
                        let name = profile.name.clone();
                        row.connect_activated(move |_| {
                            let page = detail(&state, &shell, &name);
                            let title = state
                                .profile(&name)
                                .map(|p| p.name)
                                .unwrap_or_else(|| name.clone());
                            shell.push_page(&page, &title);
                        });
                        list.append(&row);
                    }
                }
            }

            let add = adw::ActionRow::builder()
                .title("Add profile")
                .activatable(true)
                .build();
            add.add_prefix(&gtk::Image::from_icon_name("list-add-symbolic"));
            let add_state = Rc::clone(&state);
            add.connect_activated(move |row| add_profile(&add_state, row));
            list.append(&add);

            let wireguard = adw::ActionRow::builder()
                .title("Add WireGuard profile")
                .subtitle("Paste a WireGuard configuration")
                .activatable(true)
                .build();
            wireguard.add_prefix(&gtk::Image::from_icon_name("network-vpn-symbolic"));
            let wireguard_state = Rc::clone(&state);
            wireguard.connect_activated(move |row| {
                crate::views::wireguard::add_wireguard(&wireguard_state, row)
            });
            list.append(&wireguard);

            let warp = adw::ActionRow::builder()
                .title("Register WARP profile")
                .subtitle("Cloudflare WARP over MASQUE or WireGuard")
                .activatable(true)
                .build();
            warp.add_prefix(&gtk::Image::from_icon_name("network-vpn-symbolic"));
            let warp_state = Rc::clone(&state);
            warp.connect_activated(move |row| {
                crate::views::wireguard::register_warp(&warp_state, row)
            });
            list.append(&warp);
        }
    };

    refresh();
    state.subscribe(&scroller, refresh);
    scroller.upcast()
}

fn subtitle(profile: &Profile) -> String {
    if profile.removed {
        return "removed".to_string();
    }
    if !profile.last_error.is_empty() {
        return profile.last_error.clone();
    }
    let place = if profile.magic_dns_suffix.is_empty() {
        profile.hostname.clone()
    } else {
        profile.magic_dns_suffix.clone()
    };
    format!("{} · {}", profile.state_label(), place)
}

fn detail(state: &Shared, shell: &Rc<Shell>, name: &str) -> gtk::Widget {
    let (scroller, content) = app::page_body();

    let info = app::group(name);
    content.append(&info);
    let info_list = app::boxed_list();
    info.add(&info_list);

    let routes = app::group("Routing");
    content.append(&routes);
    let routes_list = app::boxed_list();
    routes.add(&routes_list);

    let actions = app::group("Actions");
    content.append(&actions);
    let actions_list = app::boxed_list();
    actions.add(&actions_list);

    let rename = action_button("Rename profile", "document-edit-symbolic");
    let toggle = action_button("Disable profile", "media-playback-pause-symbolic");
    let login = action_button("Open login link", "web-browser-symbolic");
    let restart = action_button("Restart profile", "view-refresh-symbolic");
    let wireguard = action_button("WireGuard details", "network-vpn-symbolic");
    let remove = action_button("Remove profile", "user-trash-symbolic");
    let purge = action_button("Remove and delete local state", "user-trash-symbolic");
    remove.add_css_class("destructive-action");
    purge.add_css_class("destructive-action");
    for row in [
        &rename, &toggle, &restart, &login, &wireguard, &remove, &purge,
    ] {
        actions_list.append(row);
    }

    let accept_all = adw::SwitchRow::builder()
        .title("Accept all subnet routes")
        .subtitle("Advertised routes apply without per-prefix approval")
        .build();
    routes_list.append(&accept_all);
    let accept_all_dns = adw::SwitchRow::builder()
        .title("Accept all DNS routes")
        .subtitle("Advertised DNS routes apply without per-domain approval")
        .build();
    routes_list.append(&accept_all_dns);

    let loading = Rc::new(std::cell::Cell::new(false));
    let refresh = {
        let state = Rc::clone(state);
        let name = name.to_string();
        let info_list = info_list.clone();
        let accept_all = accept_all.clone();
        let accept_all_dns = accept_all_dns.clone();
        let loading = Rc::clone(&loading);
        let toggle = toggle.clone();
        let wireguard_row = wireguard.clone();
        move || {
            let core = state.core();
            let Some(profile) = core
                .status
                .as_ref()
                .and_then(|status| status.profiles.iter().find(|p| p.name == name))
                .cloned()
            else {
                app::clear(&info_list);
                info_list.append(&app::row(
                    "Profile unavailable",
                    "tailmixd is not reachable",
                ));
                return;
            };
            app::clear(&info_list);
            for (title, value) in [
                ("Kind", profile.kind_label().to_string()),
                ("State", profile.state_label().to_string()),
                ("Hostname", profile.hostname.clone()),
                ("MagicDNS", profile.magic_dns_suffix.clone()),
                ("Self name", profile.self_dns_name.clone()),
                ("Self IPs", profile.self_ips.join(", ")),
                ("Peers", profile.peer_count.to_string()),
                ("Backend", profile.backend_state.clone()),
                ("State dir", profile.state_dir.clone()),
                ("Last error", profile.last_error.clone()),
            ] {
                if !value.is_empty() {
                    info_list.append(&app::row(title, &value));
                }
            }
            loading.set(true);
            accept_all.set_active(profile.accept_all_routes);
            accept_all_dns.set_active(profile.accept_all_dns_routes);
            loading.set(false);
            wireguard_row.set_visible(profile.kind_label() != "tailscale");
            toggle.set_title(if profile.enabled {
                "Disable profile"
            } else {
                "Enable profile"
            });
        }
    };

    refresh();
    state.subscribe(&scroller, refresh);

    {
        let state = Rc::clone(state);
        let name = name.to_string();
        let shell = Rc::clone(shell);
        rename.connect_activated(move |row| {
            let state = Rc::clone(&state);
            let shell = Rc::clone(&shell);
            let name = name.clone();
            let original = name.clone();
            dialogs::prompt(row, "Rename profile", "Name", &name, move |new_name| {
                if new_name.trim().is_empty() || new_name == original {
                    return false;
                }
                let shell = Rc::clone(&shell);
                // The page is bound to the old name, so leave it after a rename.
                state.mutate_with(
                    vec![
                        "profiles".into(),
                        "rename".into(),
                        original.clone(),
                        new_name,
                    ],
                    None,
                    move || shell.pop(),
                );
                true
            });
        });
    }

    {
        let state = Rc::clone(state);
        let name = name.to_string();
        toggle.connect_activated(move |row| {
            let enabled = state
                .profile(&name)
                .map(|profile| profile.enabled)
                .unwrap_or(true);
            let verb = if enabled { "disable" } else { "enable" };
            state.mutate(vec!["profiles".into(), verb.into(), name.clone()], None);
            let _ = row;
        });
    }

    {
        let state = Rc::clone(state);
        let name = name.to_string();
        login.connect_activated(move |row| {
            let url = state
                .profile(&name)
                .map(|profile| profile.auth_url)
                .unwrap_or_default();
            if url.is_empty() {
                state.toast("No login link: this profile is already signed in");
                return;
            }
            let launcher = gtk::UriLauncher::new(&url);
            let toast = state.clone();
            let window = row.root().and_downcast::<gtk::Window>();
            launcher.launch(
                window.as_ref(),
                gtk::gio::Cancellable::NONE,
                move |result| {
                    if result.is_err() {
                        toast.toast("Could not open the browser");
                    }
                },
            );
        });
    }

    {
        let state = Rc::clone(state);
        let name = name.to_string();
        restart.connect_activated(move |_| {
            state.mutate(
                vec!["profiles".into(), "restart".into(), name.clone()],
                None,
            );
        });
    }

    {
        let state = Rc::clone(state);
        let shell = Rc::clone(shell);
        let name = name.to_string();
        let wireguard_state = Rc::clone(&state);
        wireguard.connect_activated(move |_| {
            let page = crate::views::wireguard::detail(&wireguard_state, &name);
            shell.push_page(&page, "WireGuard");
        });
    }

    {
        let state = Rc::clone(state);
        let shell = Rc::clone(shell);
        let name = name.to_string();
        remove.connect_activated(move |row| {
            let state = Rc::clone(&state);
            let shell = Rc::clone(&shell);
            let name = name.clone();
            dialogs::confirm(
                row,
                "Remove profile?",
                "The profile stops and its routes are withdrawn. Local state is kept on disk.",
                "Remove",
                move || {
                    let shell = Rc::clone(&shell);
                    state.mutate_with(
                        vec!["profiles".into(), "remove".into(), name.clone()],
                        None,
                        move || shell.pop(),
                    );
                },
            );
        });
    }

    {
        let state = Rc::clone(state);
        let shell = Rc::clone(shell);
        let name = name.to_string();
        purge.connect_activated(move |row| {
            let state = Rc::clone(&state);
            let shell = Rc::clone(&shell);
            let name = name.clone();
            let state_dir = state
                .profile(&name)
                .map(|profile| profile.state_dir)
                .unwrap_or_default();
            dialogs::confirm(
                row,
                "Remove and delete local state?",
                &format!("The profile stops and {state_dir} is deleted. This cannot be undone."),
                "Delete",
                move || {
                    let shell = Rc::clone(&shell);
                    state.mutate_with(
                        vec![
                            "profiles".into(),
                            "remove".into(),
                            "--purge".into(),
                            "--yes".into(),
                            name.clone(),
                        ],
                        None,
                        move || shell.pop(),
                    );
                },
            );
        });
    }

    {
        let state = Rc::clone(state);
        let name = name.to_string();
        let loading = Rc::clone(&loading);
        accept_all.connect_active_notify(move |row| {
            if loading.get() {
                return;
            }
            let value = if row.is_active() { "true" } else { "false" };
            state.mutate(
                vec![
                    "routes".into(),
                    "set".into(),
                    "-p".into(),
                    name.clone(),
                    format!("--accept-all={value}"),
                ],
                None,
            );
        });
    }

    {
        let state = Rc::clone(state);
        let name = name.to_string();
        let loading = Rc::clone(&loading);
        accept_all_dns.connect_active_notify(move |row| {
            if loading.get() {
                return;
            }
            let value = if row.is_active() { "true" } else { "false" };
            state.mutate(
                vec![
                    "dns".into(),
                    "routes".into(),
                    "set".into(),
                    "-p".into(),
                    name.clone(),
                    format!("--accept-all={value}"),
                ],
                None,
            );
        });
    }

    let view = adw::ToolbarView::new();
    let header = adw::HeaderBar::new();
    view.add_top_bar(&header);
    view.set_content(Some(&scroller));
    view.upcast()
}

fn action_button(title: &str, icon: &str) -> adw::ActionRow {
    let row = adw::ActionRow::builder()
        .title(title)
        .activatable(true)
        .build();
    row.add_prefix(&gtk::Image::from_icon_name(icon));
    row
}

fn add_profile(state: &Shared, parent: &impl IsA<gtk::Widget>) {
    let form = dialogs::form("Add profile", "Add", 420);
    let list = gtk::ListBox::builder()
        .selection_mode(gtk::SelectionMode::None)
        .css_classes(["boxed-list"])
        .build();
    let name = adw::EntryRow::builder().title("Name").build();
    let hostname = adw::EntryRow::builder()
        .title("Hostname (optional)")
        .build();
    let state_dir = adw::EntryRow::builder()
        .title("State directory (optional)")
        .build();
    let auth_key = adw::PasswordEntryRow::builder()
        .title("Auth key (optional)")
        .build();
    let disabled = adw::SwitchRow::builder()
        .title("Create stopped")
        .subtitle("Start the profile later")
        .build();
    for row in [
        name.upcast_ref::<gtk::Widget>(),
        hostname.upcast_ref(),
        state_dir.upcast_ref(),
        auth_key.upcast_ref(),
        disabled.upcast_ref(),
    ] {
        list.append(row);
    }
    form.body.append(&list);

    let state = Rc::clone(state);
    form.on_accept(move || {
        let name = name.text().to_string();
        if name.trim().is_empty() {
            state.toast("A profile name is required");
            return false;
        }
        let mut args = vec!["profiles".to_string(), "add".to_string(), name];
        let hostname = hostname.text().to_string();
        if !hostname.is_empty() {
            args.push("--hostname".into());
            args.push(hostname);
        }
        let dir = state_dir.text().to_string();
        if !dir.is_empty() {
            args.push("--state-dir".into());
            args.push(dir);
        }
        let key = auth_key.text().to_string();
        if !key.is_empty() {
            args.push("--auth-key-file".into());
            args.push("-".into());
        }
        if disabled.is_active() {
            args.push("--disabled".into());
        }
        let stdin = if key.is_empty() { None } else { Some(key) };
        state.mutate(args, stdin);
        true
    });

    form.present(parent);
}
