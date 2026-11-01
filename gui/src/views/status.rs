//! Status view: one glance at the daemon, the profiles, and the live mapping.

use adw::prelude::*;
use std::rc::Rc;

use crate::app;
use crate::store::Shared;

pub fn build(state: &Shared) -> gtk::Widget {
    let (scroller, content) = app::page_body();

    let daemon_group = app::group("Daemon");
    content.append(&daemon_group);
    let daemon_list = app::boxed_list();
    let daemon_row = app::row("tailmixd", "Checking…");
    let switch = gtk::Switch::builder()
        .valign(gtk::Align::Center)
        .active(true)
        .build();
    // Refresh sets the switch too; without this guard each poll would run `tailmix up`.
    let loading = Rc::new(std::cell::Cell::new(false));
    daemon_row.add_suffix(&switch);
    daemon_row.set_activatable_widget(Some(&switch));
    daemon_list.append(&daemon_row);
    daemon_group.add(&daemon_list);

    let profiles_group = app::group("Profiles");
    content.append(&profiles_group);
    let profiles_list = app::boxed_list();
    profiles_group.add(&profiles_list);

    let routing_group = app::group("Routing");
    content.append(&routing_group);
    let routing_list = app::boxed_list();
    routing_group.add(&routing_list);

    let refresh = {
        let state = Rc::clone(state);
        let daemon_row = daemon_row.clone();
        let switch = switch.clone();
        let loading = Rc::clone(&loading);
        let profiles_list = profiles_list.clone();
        let routing_list = routing_list.clone();
        move || {
            let core = state.core();
            let (title, subtitle, up) = match (&core.status, &core.failure) {
                (Some(status), _) => (
                    "Running".to_string(),
                    format!("{} profile(s)", status.profiles.len()),
                    status.state != "down",
                ),
                (None, Some(failure)) => (
                    "Not reachable".to_string(),
                    crate::store::failure_message(failure),
                    false,
                ),
                (None, None) => ("Checking…".to_string(), String::new(), true),
            };
            daemon_row.set_title(&title);
            daemon_row.set_subtitle(&subtitle);
            loading.set(true);
            switch.set_active(up);
            loading.set(false);

            app::clear(&profiles_list);
            match core.status.as_ref() {
                None => profiles_list.append(&app::row("No data", "tailmixd is not reachable")),
                Some(status) if status.profiles.is_empty() => {
                    profiles_list.append(&app::row("No profiles", "Add one in the Profiles view"))
                }
                Some(status) => {
                    for profile in &status.profiles {
                        let subtitle = if profile.removed {
                            "removed".to_string()
                        } else if !profile.last_error.is_empty() {
                            profile.last_error.clone()
                        } else if profile.is_connected() {
                            format!("{} peer(s) · {}", profile.peer_count, profile.kind_label())
                        } else {
                            profile.kind_label().to_string()
                        };
                        let row = app::row(&profile.name, &subtitle);
                        let pill = gtk::Label::new(Some(&profile.state_label()));
                        pill.add_css_class("caption");
                        pill.add_css_class("dim-label");
                        row.add_suffix(&pill);
                        profiles_list.append(&row);
                    }
                }
            }

            app::clear(&routing_list);
            match core.status.as_ref() {
                None => routing_list.append(&app::row("No data", "")),
                Some(status) => {
                    for binding in &status.ip_routes.bindings {
                        routing_list.append(&app::row(
                            &binding.prefix,
                            &format!("{} · {}", binding.profile_name, binding.state),
                        ));
                    }
                    for binding in &status.dns_routes.bindings {
                        routing_list.append(&app::row(
                            &binding.domain,
                            &format!("{} · DNS", binding.profile_name),
                        ));
                    }
                    if let Some(selected) = &status.exit_nodes.selected {
                        routing_list.append(&app::row(
                            "Exit node",
                            &format!("{} · {}", selected.profile_name, selected.dns_name),
                        ));
                    }
                    if status.ip_routes.bindings.is_empty()
                        && status.dns_routes.bindings.is_empty()
                        && status.exit_nodes.selected.is_none()
                    {
                        routing_list.append(&app::row("Nothing mapped", "No routes or exit node"));
                    }
                }
            }
        }
    };

    refresh();
    state.subscribe(refresh);

    let state_up = Rc::clone(state);
    let loading = Rc::clone(&loading);
    switch.connect_state_notify(move |switch| {
        if loading.get() {
            return;
        }
        let args = vec![if switch.is_active() { "up" } else { "down" }.to_string()];
        state_up.mutate(args, None);
    });

    scroller.upcast()
}
