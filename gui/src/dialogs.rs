//! Small dialog helpers shared by the views.
//!
//! HIG: dialogs are modal, never stacked, and close with Escape.

use adw::prelude::*;
use std::rc::Rc;

pub struct FormDialog {
    pub dialog: adw::Dialog,
    pub body: gtk::Box,
    accept: gtk::Button,
}

/// A boxed list for rows placed inside a dialog body.
pub fn boxed_rows() -> gtk::ListBox {
    gtk::ListBox::builder()
        .selection_mode(gtk::SelectionMode::None)
        .css_classes(["boxed-list"])
        .build()
}

/// Builds a modal dialog with a header bar, a form body, and Cancel/Apply.
pub fn form(title: &str, accept: &str, width: i32) -> FormDialog {
    let body = gtk::Box::new(gtk::Orientation::Vertical, 18);
    body.set_margin_top(18);
    body.set_margin_bottom(18);
    body.set_margin_start(18);
    body.set_margin_end(18);

    let header = adw::HeaderBar::new();
    let cancel = gtk::Button::with_label("Cancel");
    header.pack_start(&cancel);
    let apply = gtk::Button::with_label(accept);
    apply.add_css_class("suggested-action");
    header.pack_end(&apply);

    let view = adw::ToolbarView::new();
    view.add_top_bar(&header);
    view.set_content(Some(&body));

    let dialog = adw::Dialog::builder()
        .title(title)
        .content_width(width)
        .child(&view)
        .build();

    let weak = dialog.downgrade();
    cancel.connect_clicked(move |_| {
        if let Some(dialog) = weak.upgrade() {
            dialog.close();
        }
    });

    FormDialog {
        dialog,
        body,
        accept: apply,
    }
}

impl FormDialog {
    /// Runs `on_accept` when Apply is pressed; returning `true` closes the dialog.
    pub fn on_accept(&self, on_accept: impl Fn() -> bool + 'static) {
        let weak = self.dialog.downgrade();
        let on_accept = Rc::new(on_accept);
        self.accept.connect_clicked(move |_| {
            if on_accept() {
                if let Some(dialog) = weak.upgrade() {
                    dialog.close();
                }
            }
        });
    }

    pub fn present(&self, parent: &impl IsA<gtk::Widget>) {
        self.dialog.present(Some(parent));
    }
}

/// A single-line text prompt.
pub fn prompt(
    parent: &impl IsA<gtk::Widget>,
    title: &str,
    label: &str,
    initial: &str,
    on_accept: impl Fn(String) -> bool + 'static,
) {
    let form = form(title, "Apply", 360);
    let entry = adw::EntryRow::builder().title(label).build();
    entry.set_text(initial);
    let list = gtk::ListBox::builder()
        .selection_mode(gtk::SelectionMode::None)
        .css_classes(["boxed-list"])
        .build();
    list.append(&entry);
    form.body.append(&list);
    let entries = entry.clone();
    form.on_accept(move || on_accept(entries.text().to_string()));
    form.present(parent);
    entry.grab_focus();
}

/// Yes/no confirmation for a destructive change.
pub fn confirm(
    parent: &impl IsA<gtk::Widget>,
    heading: &str,
    body: &str,
    accept: &str,
    on_accept: impl Fn() + 'static,
) {
    let alert = adw::AlertDialog::new(Some(heading), Some(body));
    alert.add_response("cancel", "Cancel");
    alert.add_response("ok", accept);
    alert.set_response_appearance("ok", adw::ResponseAppearance::Destructive);
    alert.set_default_response(Some("cancel"));
    alert.set_close_response("cancel");
    let on_accept = Rc::new(on_accept);
    alert.connect_response(None, move |_, response| {
        if response == "ok" {
            on_accept();
        }
    });
    alert.present(Some(parent));
}

/// Splits a comma or whitespace separated list.
pub fn split_list(text: &str) -> Vec<String> {
    text.split([',', ' ', '\t', '\n'])
        .map(str::trim)
        .filter(|item| !item.is_empty())
        .map(str::to_string)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splits_lists_on_commas_and_whitespace() {
        assert_eq!(
            split_list("10.0.0.0/8, 10.1.0.0/16"),
            vec!["10.0.0.0/8", "10.1.0.0/16"]
        );
        assert_eq!(
            split_list("  corp.example\tlab.example\n"),
            vec!["corp.example", "lab.example"]
        );
        assert!(split_list("   ").is_empty());
    }
}
