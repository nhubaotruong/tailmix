use adw::prelude::*;
use gtk::{gdk, glib};
use std::rc::Rc;

pub const APP_ID: &str = "io.github.nhubaotruong.TailmixGui";

/// Views the switcher holds. The HIG caps a view switcher at five.
pub struct Shell {
    pub window: adw::ApplicationWindow,
    pub stack: adw::ViewStack,
    pub nav: adw::NavigationView,
    pub toasts: adw::ToastOverlay,
    header: adw::HeaderBar,
    switcher_bar: adw::ViewSwitcherBar,
}

pub fn install_css() {
    let provider = gtk::CssProvider::new();
    provider.load_from_resource("/io/github/nhubaotruong/TailmixGui/style.css");
    if let Some(display) = gdk::Display::default() {
        gtk::style_context_add_provider_for_display(
            &display,
            &provider,
            gtk::STYLE_PROVIDER_PRIORITY_APPLICATION,
        );
    }
}

pub fn activate(app: &adw::Application) {
    let shell = Rc::new(build(app));
    let state = crate::store::new();
    state.set_toast_sink({
        let shell = Rc::clone(&shell);
        move |message| shell.toast(message)
    });

    let status = crate::views::status::build(&state);
    shell.add_view("status", "Status", "network-wired-symbolic", &status);
    let profiles = crate::views::profiles::build(&state, &shell);
    shell.add_view("profiles", "Profiles", "avatar-default-symbolic", &profiles);
    let network = crate::views::network::build(&state, &shell);
    shell.add_view("network", "Network", "network-workgroup-symbolic", &network);
    let system = crate::views::system::build(&state, &shell);
    shell.add_view("system", "System", "emblem-system-symbolic", &system);

    shell.stack.connect_visible_child_notify({
        let shell = Rc::clone(&shell);
        move |_| shell.sync_title()
    });
    shell.sync_title();
    state.start_polling(shell.window.clone().upcast());
    shell.window.present();
    state.refresh();
}

fn build(app: &adw::Application) -> Shell {
    let stack = adw::ViewStack::new();
    stack.set_hhomogeneous(false);

    let switcher = adw::ViewSwitcher::builder()
        .stack(&stack)
        .policy(adw::ViewSwitcherPolicy::Wide)
        .build();

    let switcher_bar = adw::ViewSwitcherBar::builder().stack(&stack).build();

    let header = adw::HeaderBar::new();
    header.set_title_widget(Some(&switcher));

    let root = adw::ToolbarView::new();
    root.add_top_bar(&header);
    root.add_bottom_bar(&switcher_bar);
    root.set_content(Some(&stack));

    let nav = adw::NavigationView::new();
    nav.add(&adw::NavigationPage::new(&root, "Tailmix"));

    let toasts = adw::ToastOverlay::new();
    toasts.set_child(Some(&nav));

    let window = adw::ApplicationWindow::builder()
        .application(app)
        .title("Tailmix")
        .default_width(900)
        .default_height(640)
        .content(&toasts)
        .build();

    // Narrow windows move the switcher to the bottom bar, which is the
    // libadwaita pattern now that ViewSwitcherPolicy::Narrow is gone.
    let condition = adw::BreakpointCondition::new_length(
        adw::BreakpointConditionLengthType::MaxWidth,
        550.0,
        adw::LengthUnit::Sp,
    );
    let breakpoint = adw::Breakpoint::new(condition);
    breakpoint.add_setters(&[(&switcher_bar, "reveal", true)]);
    let header_weak = header.downgrade();
    let switcher_weak = switcher.downgrade();
    breakpoint.connect_apply(move |_| {
        if let Some(header) = header_weak.upgrade() {
            header.set_title_widget(gtk::Widget::NONE);
        }
        if let Some(switcher) = switcher_weak.upgrade() {
            switcher.set_visible(false);
        }
    });
    let header_weak = header.downgrade();
    let switcher_weak = switcher.downgrade();
    breakpoint.connect_unapply(move |_| {
        if let Some(switcher) = switcher_weak.upgrade() {
            switcher.set_visible(true);
            if let Some(header) = header_weak.upgrade() {
                header.set_title_widget(Some(&switcher));
            }
        }
    });
    window.add_breakpoint(breakpoint);

    Shell {
        window,
        stack,
        nav,
        toasts,
        header,
        switcher_bar,
    }
}

impl Shell {
    pub fn add_view(&self, name: &str, title: &str, icon: &str, child: &impl IsA<gtk::Widget>) {
        self.stack
            .add_titled_with_icon(child, Some(name), title, icon);
    }

    /// Pushes a plain widget as a new page with its own header title.
    pub fn push_page(&self, child: &impl IsA<gtk::Widget>, title: &str) {
        self.nav.push(&adw::NavigationPage::new(child, title));
    }

    pub fn pop(&self) {
        self.nav.pop();
    }

    pub fn toast(&self, title: &str) {
        self.toasts.add_toast(adw::Toast::new(title));
    }

    /// Keeps the compositor title in sync with the visible view.
    pub fn sync_title(&self) {
        let title = self
            .stack
            .visible_child_name()
            .map(|name| match name.as_str() {
                "status" => "Status",
                "profiles" => "Profiles",
                "network" => "Network",
                "system" => "System",
                _ => "Tailmix",
            })
            .unwrap_or("Tailmix");
        self.window.set_title(Some(title));
    }

    #[allow(dead_code)]
    pub fn header(&self) -> &adw::HeaderBar {
        &self.header
    }

    #[allow(dead_code)]
    pub fn switcher_bar(&self) -> &adw::ViewSwitcherBar {
        &self.switcher_bar
    }
}

/// Standard empty state for a view that has nothing to show yet.
/// Scrolled, width-clamped page body, the shape every view uses.
pub fn page_body() -> (gtk::ScrolledWindow, gtk::Box) {
    let content = gtk::Box::new(gtk::Orientation::Vertical, 18);
    content.set_margin_top(18);
    content.set_margin_bottom(24);
    content.set_margin_start(12);
    content.set_margin_end(12);

    let clamp = adw::Clamp::builder()
        .maximum_size(720)
        .tightening_threshold(600)
        .child(&content)
        .build();

    let scroller = gtk::ScrolledWindow::builder()
        .hscrollbar_policy(gtk::PolicyType::Never)
        .vexpand(true)
        .child(&clamp)
        .build();

    (scroller, content)
}

pub fn group(title: &str) -> adw::PreferencesGroup {
    adw::PreferencesGroup::builder().title(title).build()
}

pub fn boxed_list() -> gtk::ListBox {
    gtk::ListBox::builder()
        .selection_mode(gtk::SelectionMode::None)
        .css_classes(["boxed-list"])
        .build()
}

pub fn row(title: &str, subtitle: &str) -> adw::ActionRow {
    let row = adw::ActionRow::builder().title(title).build();
    if !subtitle.is_empty() {
        row.set_subtitle(subtitle);
    }
    row
}

/// Empties a list before it is rebuilt from fresh state.
pub fn clear(list: &gtk::ListBox) {
    let mut child = list.first_child();
    while let Some(widget) = child {
        child = widget.next_sibling();
        list.remove(&widget);
    }
}

/// Applies the colour scheme the preferences ask for.
pub fn apply_color_scheme(scheme: &str) {
    let manager = adw::StyleManager::default();
    manager.set_color_scheme(match scheme {
        "light" => adw::ColorScheme::ForceLight,
        "dark" => adw::ColorScheme::ForceDark,
        _ => adw::ColorScheme::Default,
    });
}

#[allow(dead_code)]
pub fn spawn_app_quit(app: &adw::Application) {
    let app = app.clone();
    glib::spawn_future_local(async move {
        app.quit();
    });
}
