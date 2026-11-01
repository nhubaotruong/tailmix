mod app;

use adw::prelude::*;
use gtk::glib;

fn main() -> glib::ExitCode {
    let app = adw::Application::builder()
        .application_id(app::APP_ID)
        .flags(gtk::gio::ApplicationFlags::HANDLES_COMMAND_LINE)
        .build();

    app.connect_startup(|_| app::install_css());
    app.connect_activate(|app| app::activate(app));
    app.connect_command_line(|app, _| {
        app.activate();
        0.into()
    });

    app.run()
}
