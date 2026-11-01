mod app;
mod bridge;
mod dialogs;
mod models;
mod store;
mod views;

use adw::prelude::*;
use gtk::glib;

fn main() -> glib::ExitCode {
    gtk::gio::resources_register_include!("tailmix-gui.gresource").expect("register resources");

    if running_as_root() {
        eprintln!(
            "tailmix-gui: run as your desktop user; the app asks for authorization per change."
        );
        return glib::ExitCode::FAILURE;
    }

    let app = adw::Application::builder()
        .application_id(app::APP_ID)
        .flags(gtk::gio::ApplicationFlags::HANDLES_COMMAND_LINE)
        .build();

    app.connect_startup(|_| app::install_css());
    app.connect_activate(app::activate);
    app.connect_command_line(|app, _| {
        app.activate();
        0.into()
    });

    app.run()
}

/// A GUI owned by root breaks the desktop session and its agent; refuse it.
fn running_as_root() -> bool {
    use std::os::unix::fs::MetadataExt;
    std::fs::metadata("/proc/self")
        .map(|meta| meta.uid() == 0)
        .unwrap_or(false)
}
