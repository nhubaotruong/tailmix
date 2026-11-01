//! Network view: subnet routes, the exit node, DNS routes, and search domains.

use adw::prelude::*;
use gtk::{gio, glib};
use std::rc::Rc;

use crate::app::{self, Shell};
use crate::bridge::Failure;
use crate::dialogs;
use crate::models::{ExitNodes, IpRoutes, Profile};
use crate::store::Shared;

pub fn build(state: &Shared, shell: &Rc<Shell>) -> gtk::Widget {
    let (scroller, content) = app::page_body();

    let ip_group = app::group("Subnet routes");
    content.append(&ip_group);
    let ip_list = app::boxed_list();
    ip_group.add(&ip_list);

    let exit_group = app::group("Exit node");
    content.append(&exit_group);
    let exit_list = app::boxed_list();
    exit_group.add(&exit_list);

    let dns_group = app::group("DNS routes");
    content.append(&dns_group);
    let dns_list = app::boxed_list();
    dns_group.add(&dns_list);

    let search_group = app::group("Search domains");
    content.append(&search_group);
    let search_list = app::boxed_list();
    search_group.add(&search_list);

    let refresh = {
        let state = Rc::clone(state);
        let shell = Rc::clone(shell);
        let ip_list = ip_list.clone();
        let exit_list = exit_list.clone();
        let dns_list = dns_list.clone();
        let search_list = search_list.clone();
        move || {
            let core = state.core();
            let Some(status) = core.status.as_ref() else {
                for list in [&ip_list, &exit_list, &dns_list, &search_list] {
                    app::clear(list);
                    list.append(&app::row("tailmixd is not running", ""));
                }
                return;
            };

            app::clear(&ip_list);
            for binding in &status.ip_routes.bindings {
                let row = app::row(
                    &binding.prefix,
                    &format!("{} · {}", binding.profile_name, binding.state),
                );
                let state = Rc::clone(&state);
                let prefix = binding.prefix.clone();
                let button = gtk::Button::from_icon_name("window-close-symbolic");
                button.add_css_class("flat");
                button.set_tooltip_text(Some("Stop routing this prefix"));
                button.connect_clicked(move |_| {
                    state.mutate(vec!["routes".into(), "unbind".into(), prefix.clone()], None);
                });
                row.add_suffix(&button);
                ip_list.append(&row);
            }
            ip_list.append(&action("Bind subnet route", "list-add-symbolic", {
                let state = Rc::clone(&state);
                move |row| bind_dialog(&state, row, false)
            }));
            append_problem(&ip_list, &status.ip_routes.reconcile_error);
            ip_list.append(&action(
                "Browse advertised routes",
                "network-workgroup-symbolic",
                {
                    let state = Rc::clone(&state);
                    let shell = Rc::clone(&shell);
                    move |_| {
                        let page = browse_page(&state, &shell, Browse::Routes);
                        shell.push_page(&page, "Advertised routes");
                    }
                },
            ));

            app::clear(&exit_list);
            match &status.exit_nodes.selected {
                Some(selected) => {
                    let row = app::row(
                        &selected.dns_name,
                        &format!("{} · {}", selected.profile_name, selected.state),
                    );
                    let state = Rc::clone(&state);
                    let clear = gtk::Button::with_label("Clear");
                    clear.add_css_class("flat");
                    clear.connect_clicked(move |_| {
                        state.mutate(vec!["exit-node".into(), "clear".into()], None);
                    });
                    row.add_suffix(&clear);
                    exit_list.append(&row);
                }
                None => exit_list.append(&app::row(
                    "No exit node",
                    "Default traffic leaves through this machine",
                )),
            }
            exit_list.append(&action("Choose exit node", "go-next-symbolic", {
                let state = Rc::clone(&state);
                let shell = Rc::clone(&shell);
                move |_| {
                    let page = browse_page(&state, &shell, Browse::ExitNodes);
                    shell.push_page(&page, "Exit nodes");
                }
            }));
            append_problem(&exit_list, &status.exit_nodes.reconcile_error);

            app::clear(&dns_list);
            for binding in &status.dns_routes.bindings {
                let row = app::row(
                    &binding.domain,
                    &format!("{} · {}", binding.profile_name, binding.state),
                );
                let state = Rc::clone(&state);
                let domain = binding.domain.clone();
                let button = gtk::Button::from_icon_name("window-close-symbolic");
                button.add_css_class("flat");
                button.set_tooltip_text(Some("Stop routing this domain"));
                button.connect_clicked(move |_| {
                    state.mutate(
                        vec![
                            "dns".into(),
                            "routes".into(),
                            "unbind".into(),
                            domain.clone(),
                        ],
                        None,
                    );
                });
                row.add_suffix(&button);
                dns_list.append(&row);
            }
            dns_list.append(&action("Bind DNS route", "list-add-symbolic", {
                let state = Rc::clone(&state);
                move |row| bind_dialog(&state, row, true)
            }));
            append_problem(&dns_list, &status.dns_routes.reconcile_error);

            app::clear(&search_list);
            for domain in &status.search_domains.desired {
                search_list.append(&app::row(domain, "Configured"));
            }
            for waiting in &status.search_domains.waiting {
                search_list.append(&app::row(&waiting.domain, &waiting.reason));
            }
            if status.search_domains.desired.is_empty() && status.search_domains.waiting.is_empty()
            {
                search_list.append(&app::row(
                    "No search domains",
                    "Detected and configured domains appear here",
                ));
            }
            search_list.append(&action("Add search domain", "list-add-symbolic", {
                let state = Rc::clone(&state);
                move |row| add_search_domain(&state, row)
            }));
            search_list.append(&action("Clear search domains", "edit-clear-symbolic", {
                let state = Rc::clone(&state);
                move |row| {
                    let state = Rc::clone(&state);
                    dialogs::confirm(
                        row,
                        "Clear search domains?",
                        "Configured search domains are removed. Detected domains stay.",
                        "Clear",
                        move || {
                            state.mutate(vec!["dns".into(), "search".into(), "clear".into()], None);
                        },
                    );
                }
            }));
            append_problem(&search_list, &status.search_domains.reconcile_error);
        }
    };

    refresh();
    state.subscribe(refresh);
    scroller.upcast()
}

fn action(
    title: &str,
    icon: &str,
    on_activated: impl Fn(&adw::ActionRow) + 'static,
) -> adw::ActionRow {
    let row = adw::ActionRow::builder()
        .title(title)
        .activatable(true)
        .build();
    row.add_prefix(&gtk::Image::from_icon_name(icon));
    row.connect_activated(on_activated);
    row
}

/// A reconcile failure is persistent state, so it sits in the list, not a toast.
fn append_problem(list: &gtk::ListBox, message: &str) {
    if message.is_empty() {
        return;
    }
    let row = adw::ActionRow::builder()
        .title("Could not apply every rule")
        .subtitle(message)
        .build();
    row.add_prefix(&gtk::Image::from_icon_name("dialog-warning-symbolic"));
    list.append(&row);
}

fn profiles(state: &Shared) -> Vec<Profile> {
    state
        .core()
        .status
        .as_ref()
        .map(|status| {
            status
                .profiles
                .iter()
                .filter(|profile| !profile.removed)
                .cloned()
                .collect()
        })
        .unwrap_or_default()
}

fn bind_dialog(state: &Shared, parent: &impl IsA<gtk::Widget>, dns: bool) {
    let form = dialogs::form(
        if dns {
            "Bind DNS route"
        } else {
            "Bind subnet route"
        },
        "Bind",
        420,
    );
    let list = gtk::ListBox::builder()
        .selection_mode(gtk::SelectionMode::None)
        .css_classes(["boxed-list"])
        .build();
    let entry = adw::EntryRow::builder()
        .title(if dns { "Domain" } else { "Prefix" })
        .build();
    let candidates = profiles(state);
    let names = gtk::StringList::new(
        &candidates
            .iter()
            .map(|profile| profile.name.as_str())
            .collect::<Vec<_>>(),
    );
    let combo = adw::ComboRow::builder()
        .title("Profile")
        .model(&names)
        .build();
    if !candidates.is_empty() {
        combo.set_selected(0);
    }
    list.append(&entry);
    list.append(&combo);
    form.body.append(&list);

    let state = Rc::clone(state);
    let profile_names: Vec<String> = candidates.into_iter().map(|p| p.name).collect();
    form.on_accept(move || {
        let value = entry.text().trim().to_string();
        if value.is_empty() {
            state.toast("A prefix or domain is required");
            return false;
        }
        let index = combo.selected() as usize;
        let Some(profile) = profile_names.get(index).cloned() else {
            state.toast("Add a profile first");
            return false;
        };
        let mut args: Vec<String> = if dns {
            vec!["dns".into(), "routes".into(), "bind".into()]
        } else {
            vec!["routes".into(), "bind".into()]
        };
        args.push("-p".into());
        args.push(profile);
        for item in dialogs::split_list(&value) {
            args.push(item);
        }
        state.mutate(args, None);
        true
    });
    form.present(parent);
}

fn add_search_domain(state: &Shared, parent: &impl IsA<gtk::Widget>) {
    let state = Rc::clone(state);
    dialogs::prompt(parent, "Add search domain", "Domain", "", move |value| {
        let domains = dialogs::split_list(&value);
        if domains.is_empty() {
            return false;
        }
        let mut args = vec!["dns".to_string(), "search".into(), "add".into()];
        args.extend(domains);
        state.mutate(args, None);
        true
    });
}

#[derive(Clone, Copy)]
enum Browse {
    Routes,
    ExitNodes,
}

fn browse_page(state: &Shared, shell: &Rc<Shell>, kind: Browse) -> gtk::Widget {
    let (scroller, content) = app::page_body();
    let group = app::group(match kind {
        Browse::Routes => "Advertised subnet routes",
        Browse::ExitNodes => "Available exit nodes",
    });
    content.append(&group);
    let list = app::boxed_list();
    let spinner = adw::Spinner::new();
    spinner.set_margin_top(12);
    spinner.set_margin_bottom(12);
    group.add(&spinner);
    let filter = gtk::SearchEntry::new();
    filter.set_placeholder_text(Some("Filter by country or city"));
    filter.set_visible(matches!(kind, Browse::ExitNodes));
    group.add(&filter);
    group.add(&list);

    let view = adw::ToolbarView::new();
    view.add_top_bar(&adw::HeaderBar::new());
    view.set_content(Some(&scroller));

    let list_handle = list.clone();
    let spinner_handle = spinner.clone();
    let state = Rc::clone(state);
    let shell = Rc::clone(shell);
    let bridge = state.bridge.clone();
    glib::spawn_future_local(async move {
        let fetched: Result<Vec<Choice>, Failure> = gio::spawn_blocking(move || match kind {
            Browse::Routes => bridge
                .get::<IpRoutes>("/v1/routes/available")
                .map(|routes| {
                    routes
                        .available
                        .into_iter()
                        .map(|route| Choice::from(BrowseItem::Route(route)))
                        .collect()
                }),
            Browse::ExitNodes => bridge.get::<ExitNodes>("/v1/exit-node").map(|nodes| {
                nodes
                    .available
                    .into_iter()
                    .map(|node| Choice::from(BrowseItem::Exit(node)))
                    .collect()
            }),
        })
        .await
        .unwrap_or_else(|_| Err(Failure::internal("The command did not finish.")));

        spinner_handle.set_visible(false);
        let all = match fetched {
            Ok(choices) => Rc::new(choices),
            Err(failure) => {
                list_handle.append(&app::row("Could not load the list", &failure.message));
                return;
            }
        };
        if all.is_empty() {
            list_handle.append(&app::row("Nothing advertised", "No peer offers this"));
            return;
        }
        let render: Rc<dyn Fn()> = {
            let all = Rc::clone(&all);
            let list = list_handle.clone();
            let filter = filter.clone();
            let state = Rc::clone(&state);
            let shell = Rc::clone(&shell);
            Rc::new(move || {
                app::clear(&list);
                let needle = filter.text().to_lowercase();
                let mut shown = 0;
                for choice in all.iter() {
                    if !needle.is_empty()
                        && !format!("{} {}", choice.title, choice.subtitle)
                            .to_lowercase()
                            .contains(&needle)
                    {
                        continue;
                    }
                    shown += 1;
                    let row = app::row(&choice.title, &choice.subtitle);
                    row.add_suffix(&gtk::Image::from_icon_name("go-next-symbolic"));
                    let state = Rc::clone(&state);
                    let shell = Rc::clone(&shell);
                    let args = choice.args.clone();
                    row.connect_activated(move |_| {
                        state.mutate(args.clone(), None);
                        shell.pop();
                    });
                    list.append(&row);
                }
                if shown == 0 {
                    list.append(&app::row("Nothing matches", "Change the filter"));
                }
            })
        };
        render();
        let rerender = Rc::clone(&render);
        filter.connect_search_changed(move |_| rerender());
    });

    view.upcast()
}

struct Choice {
    title: String,
    subtitle: String,
    args: Vec<String>,
}

enum BrowseItem {
    Route(crate::models::AvailableIpRoute),
    Exit(crate::models::AvailableExitNode),
}

impl From<BrowseItem> for Choice {
    fn from(item: BrowseItem) -> Self {
        match item {
            BrowseItem::Route(route) => Choice {
                title: route.prefix.clone(),
                subtitle: format!("{} · {}", route.profile_name, route.primary_router),
                args: vec![
                    "routes".to_string(),
                    "bind".into(),
                    "-p".into(),
                    route.profile_name,
                    route.prefix,
                ],
            },
            BrowseItem::Exit(node) => Choice {
                title: node.dns_name.clone(),
                subtitle: match node.location.as_ref() {
                    Some(location) => format!("{} · {}", node.profile_name, location.label()),
                    None => node.profile_name.clone(),
                },
                args: vec![
                    "exit-node".to_string(),
                    "set".into(),
                    "-p".into(),
                    node.profile_name,
                    node.dns_name,
                ],
            },
        }
    }
}
