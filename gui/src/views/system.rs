//! System view: daemon, updates, appearance, about, and `tailmix ts` passthrough.

use adw::prelude::*;
use gtk::{gio, glib};
use std::rc::Rc;

use crate::app::{self, Shell};
use crate::store::{failure_message, Shared};

pub fn build(state: &Shared, _shell: &Rc<Shell>) -> gtk::Widget {
    let (scroller, content) = app::page_body();

    let daemon_group = app::group("Daemon");
    content.append(&daemon_group);
    let daemon_list = app::boxed_list();
    daemon_group.add(&daemon_list);

    let start = adw::ActionRow::builder()
        .title("Start tailmixd")
        .subtitle("Changes need a running daemon")
        .activatable(true)
        .build();
    start.add_prefix(&gtk::Image::from_icon_name("media-playback-start-symbolic"));
    start.connect_activated({
        let state = Rc::clone(state);
        move |_| state.start_daemon()
    });
    daemon_list.append(&start);

    let update_group = app::group("Updates");
    content.append(&update_group);
    let update_list = app::boxed_list();
    update_group.add(&update_list);

    let appearance_group = app::group("Appearance");
    content.append(&appearance_group);
    let appearance_list = app::boxed_list();
    appearance_group.add(&appearance_list);
    let scheme = adw::ComboRow::builder()
        .title("Theme")
        .subtitle("Follows the system by default")
        .model(&gtk::StringList::new(&["System", "Light", "Dark"]))
        .build();
    scheme.set_selected(0);
    scheme.connect_selected_notify(|row| {
        app::apply_color_scheme(match row.selected() {
            1 => "light",
            2 => "dark",
            _ => "default",
        });
    });
    appearance_list.append(&scheme);

    let about_group = app::group("About");
    content.append(&about_group);
    let about_list = app::boxed_list();
    about_group.add(&about_list);
    let about = app::row("About Tailmix", env!("CARGO_PKG_VERSION"));
    about.add_suffix(&gtk::Image::from_icon_name("go-next-symbolic"));
    about.set_activatable(true);
    about.connect_activated(|row| {
        let dialog = adw::AboutDialog::builder()
            .application_name("Tailmix")
            .application_icon(crate::app::APP_ID)
            .version(env!("CARGO_PKG_VERSION"))
            .developer_name("Maisem Ali")
            .website("https://github.com/maisem/tailmix")
            .build();
        dialog.present(Some(row));
    });
    about_list.append(&about);

    let console_group = app::group("Tailscale command");
    console_group.set_description(Some(
        "Runs `tailmix ts` as you. Commands that need a terminal, such as login, report their failure.",
    ));
    content.append(&console_group);
    let console_list = app::boxed_list();
    console_group.add(&console_list);

    let command = adw::EntryRow::builder()
        .title("Arguments")
        .text("status")
        .build();
    command.set_show_apply_button(true);
    let output = gtk::Label::builder()
        .xalign(0.0)
        .wrap(true)
        .selectable(true)
        .css_classes(["tailmix-mono"])
        .build();
    output.set_margin_top(6);
    output.set_margin_bottom(6);
    output.set_margin_start(12);
    output.set_margin_end(12);
    console_list.append(&command);
    console_list.append(&output);

    command.connect_apply({
        let state = Rc::clone(state);
        let output = output.clone();
        move |row| {
            let typed = row.text();
            let words: Vec<String> = typed.split_whitespace().map(str::to_string).collect();
            if let Some(reason) = interactive_reason(&words) {
                output.set_label(&reason);
                return;
            }
            let mut args = vec!["ts".to_string()];
            args.extend(words);
            let bridge = state.bridge.clone();
            let output = output.clone();
            output.set_label("Running…");
            glib::spawn_future_local(async move {
                let result = gio::spawn_blocking(move || bridge.run_raw(&args)).await;
                let text = match result {
                    Ok(Ok(text)) => text,
                    Ok(Err(failure)) => failure_message(&failure),
                    Err(_) => "The command did not finish".to_string(),
                };
                output.set_label(if text.trim().is_empty() {
                    "The command printed nothing."
                } else {
                    text.trim()
                });
            });
        }
    });

    let refresh = {
        let state = Rc::clone(state);
        let daemon_list = daemon_list.clone();
        let update_list = update_list.clone();
        move || {
            let core = state.core();
            app::clear(&daemon_list);
            let running = core.status.is_some();
            daemon_list.append(&app::row(
                if running {
                    "tailmixd is running"
                } else {
                    "tailmixd is stopped"
                },
                match &core.failure {
                    Some(failure) if !running => &failure.message,
                    _ => "Reads come from the control socket; changes ask for authorisation.",
                },
            ));
            daemon_list.append(&start.clone());

            app::clear(&update_list);
            match &core.update {
                Some(update) => {
                    update_list.append(&app::row("Current version", &update.current_version));
                    if !update.available_version.is_empty() {
                        update_list
                            .append(&app::row("Available version", &update.available_version));
                    }
                    update_list.append(&app::row("State", &update.state));
                    if !update.last_error.is_empty() {
                        update_list.append(&app::row("Last error", &update.last_error));
                    }
                    let enable = if update.enabled {
                        (
                            "Disable update checks",
                            "preferences-system-symbolic",
                            "disable",
                        )
                    } else {
                        (
                            "Enable update checks",
                            "preferences-system-symbolic",
                            "enable",
                        )
                    };
                    update_list.append(&action(&state, enable.0, enable.1, &["update", enable.2]));
                    update_list.append(&action(
                        &state,
                        "Check now",
                        "view-refresh-symbolic",
                        &["update", "check"],
                    ));
                    update_list.append(&action(
                        &state,
                        "Apply available update",
                        "software-update-available-symbolic",
                        &["update", "apply"],
                    ));
                }
                None => update_list.append(&app::row(
                    "Unknown",
                    "Start tailmixd to read the update state",
                )),
            }
        }
    };

    refresh();
    state.subscribe(refresh);
    scroller.upcast()
}

fn action(state: &Shared, title: &str, icon: &str, args: &[&str]) -> adw::ActionRow {
    let row = adw::ActionRow::builder()
        .title(title)
        .activatable(true)
        .build();
    row.add_prefix(&gtk::Image::from_icon_name(icon));
    let state = Rc::clone(state);
    let args: Vec<String> = args.iter().map(|arg| arg.to_string()).collect();
    row.connect_activated(move |_| state.mutate(args.clone(), None));
    row
}

/// Commands that need a terminal or a browser the app cannot give them.
fn interactive_reason(words: &[String]) -> Option<String> {
    let first = words.first()?.as_str();
    let needs_terminal = matches!(first, "login" | "up");
    let needs_browser = first == "status"
        && words
            .iter()
            .any(|word| word == "--web" || word == "--browser");
    if !needs_terminal && !needs_browser {
        return None;
    }
    Some(format!(
        "Run this in a terminal, it needs a prompt or a browser:\ntailmix ts {}",
        words.join(" ")
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn words(text: &str) -> Vec<String> {
        text.split_whitespace().map(str::to_string).collect()
    }

    #[test]
    fn sends_interactive_commands_to_a_terminal() {
        assert!(interactive_reason(&words("login")).is_some());
        assert!(interactive_reason(&words("up --accept-routes")).is_some());
        assert!(interactive_reason(&words("status --web")).is_some());
    }

    #[test]
    fn runs_plain_commands_in_the_app() {
        assert!(interactive_reason(&words("status --json")).is_none());
        assert!(interactive_reason(&words("ping macbook")).is_none());
        assert!(interactive_reason(&[]).is_none());
    }
}
