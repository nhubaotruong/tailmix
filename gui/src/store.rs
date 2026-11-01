//! Shared client state: one poller, one in-flight mutation, many listeners.
//!
//! The daemon has no event stream (controlapi only serves request/response),
//! so the GUI polls `/v1/status` every two seconds while a window is visible.
//! Mutations are serialised because each one raises a polkit prompt and a
//! poll landing mid-change would repaint stale state over a fresh result.

use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::time::Duration;

use gtk::glib;
use gtk::prelude::*;

use crate::bridge::{Bridge, Failure, FailureKind};
use crate::models::{Status, UpdateStatus};
use adw::gio;

const POLL_INTERVAL: Duration = Duration::from_secs(2);

#[derive(Default, Clone, PartialEq)]
pub struct Core {
    pub status: Option<Status>,
    pub update: Option<UpdateStatus>,
    /// Last failure of a read; cleared by the next successful read.
    pub failure: Option<Failure>,
}

pub struct AppState {
    pub bridge: Bridge,
    core: RefCell<Core>,
    listeners: ListenerList,
    next_listener: Cell<u64>,
    polling: Cell<bool>,
    refresh_wanted: Cell<bool>,
    pub mutating: Cell<bool>,
    toast_sink: RefCell<Option<ToastSink>>,
}

type ToastSink = Rc<dyn Fn(&str)>;
type ListenerList = Rc<RefCell<Vec<(u64, Rc<dyn Fn()>)>>>;

pub type Shared = Rc<AppState>;

/// Builds the process-wide state.
pub fn new() -> Shared {
    AppState::new()
}

impl AppState {
    pub fn new() -> Shared {
        Rc::new(AppState {
            bridge: Bridge::discover(),
            core: RefCell::new(Core::default()),
            listeners: Rc::new(RefCell::new(Vec::new())),
            next_listener: Cell::new(0),
            polling: Cell::new(false),
            refresh_wanted: Cell::new(false),
            mutating: Cell::new(false),
            toast_sink: RefCell::new(None),
        })
    }

    pub fn core(&self) -> std::cell::Ref<'_, Core> {
        self.core.borrow()
    }

    pub fn profile(&self, name: &str) -> Option<crate::models::Profile> {
        self.core
            .borrow()
            .status
            .as_ref()
            .and_then(|status| status.profiles.iter().find(|p| p.name == name).cloned())
    }

    /// Registers a repaint callback that is dropped with `owner`.
    pub fn subscribe(&self, owner: &impl IsA<gtk::Widget>, listener: impl Fn() + 'static) {
        let id = self.next_listener.get();
        self.next_listener.set(id + 1);
        self.listeners.borrow_mut().push((id, Rc::new(listener)));
        let listeners = Rc::clone(&self.listeners);
        owner.connect_destroy(move |_| {
            listeners.borrow_mut().retain(|(kept, _)| *kept != id);
        });
    }

    pub fn notify(&self) {
        let listeners: Vec<Rc<dyn Fn()>> = self
            .listeners
            .borrow()
            .iter()
            .map(|(_, listener)| Rc::clone(listener))
            .collect();
        for listener in listeners {
            listener();
        }
    }

    /// Reads status and update state in the background, then repaints.
    pub fn refresh(self: &Shared) {
        if self.polling.get() {
            // A change finished while a poll was in flight; run again after it.
            self.refresh_wanted.set(true);
            return;
        }
        self.polling.set(true);
        let bridge = self.bridge.clone();
        let state = Rc::clone(self);
        glib::spawn_future_local(async move {
            let result = gio::spawn_blocking(move || {
                let status: Result<Status, Failure> = bridge.get("/v1/status");
                let update: Result<UpdateStatus, Failure> = bridge.get("/v1/update");
                (status, update)
            })
            .await;
            state.polling.set(false);
            if let Ok((status, update)) = result {
                let changed = {
                    let mut core = state.core.borrow_mut();
                    let before = core.clone();
                    match status {
                        Ok(status) => {
                            core.status = Some(status);
                            core.failure = None;
                        }
                        Err(err) => {
                            // A stale snapshot shown as healthy hides a broken daemon.
                            core.status = None;
                            core.failure = Some(err);
                        }
                    }
                    if let Ok(update) = update {
                        core.update = Some(update);
                    }
                    *core != before
                };
                // Repainting an unchanged tree resets focus and scroll every poll.
                if changed {
                    state.notify();
                }
            }
            if state.refresh_wanted.take() {
                state.refresh();
            }
        });
    }

    /// Starts the poll loop. Stops itself while a mutation is pending.
    pub fn start_polling(self: &Shared, window: gtk::Window) {
        let state = Rc::clone(self);
        glib::timeout_add_seconds_local(POLL_INTERVAL.as_secs() as u32, move || {
            let active = window.is_mapped() && !state.mutating.get();
            if active {
                state.refresh();
            }
            glib::ControlFlow::Continue
        });
    }

    /// Runs one mutation under pkexec, then refreshes.
    pub fn mutate(self: &Shared, args: Vec<String>, stdin: Option<String>) {
        self.mutate_with(args, stdin, || {});
    }

    /// Runs one mutation, then calls `after` only when it succeeded.
    pub fn mutate_with(
        self: &Shared,
        args: Vec<String>,
        stdin: Option<String>,
        after: impl Fn() + 'static,
    ) {
        if self.mutating.get() {
            self.toast("A change is still running");
            return;
        }
        self.mutating.set(true);
        let bridge = self.bridge.clone();
        let state = Rc::clone(self);
        let after: Rc<dyn Fn()> = Rc::new(after);
        glib::spawn_future_local(async move {
            let result = gio::spawn_blocking(move || bridge.mutate(&args, stdin.as_deref())).await;
            state.finish(result, "Change applied", after);
        });
    }

    /// Starts the daemon through systemd, then refreshes.
    pub fn start_daemon(self: &Shared) {
        if self.mutating.get() {
            self.toast("A change is still running");
            return;
        }
        self.mutating.set(true);
        let bridge = self.bridge.clone();
        let state = Rc::clone(self);
        glib::spawn_future_local(async move {
            let result = gio::spawn_blocking(move || bridge.start_daemon()).await;
            state.finish(result, "tailmixd started", Rc::new(|| {}));
        });
    }

    fn finish(
        self: &Shared,
        result: Result<Result<String, Failure>, Box<dyn std::any::Any + Send>>,
        success: &str,
        after: Rc<dyn Fn()>,
    ) {
        self.mutating.set(false);
        let applied = matches!(result, Ok(Ok(_)));
        let message = match result {
            Ok(Ok(_)) => success.to_string(),
            Ok(Err(err)) => failure_message(&err),
            Err(_) => "The command did not finish".to_string(),
        };
        let message = Rc::new(message);
        let state = Rc::clone(self);
        glib::idle_add_local_once(move || {
            state.refresh();
            state.notify();
            state.toast(&message);
            if applied {
                after();
            }
        });
    }

    pub fn toast(&self, message: &str) {
        if let Some(sink) = self.toast_sink.borrow().as_ref() {
            sink(message);
        }
    }

    pub fn set_toast_sink(&self, sink: impl Fn(&str) + 'static) {
        *self.toast_sink.borrow_mut() = Some(Rc::new(sink));
    }
}

/// Message shown to the user for a failed read or change.
pub fn failure_message(failure: &Failure) -> String {
    match failure.kind {
        FailureKind::Unavailable => "tailmixd is not running".to_string(),
        FailureKind::Denied => failure.message.clone(),
        // The code names the kind of refusal, which the prose often leaves out.
        FailureKind::Failed if !failure.code.is_empty() && failure.code != "internal" => {
            format!("{} ({})", failure.message, failure.code)
        }
        FailureKind::Failed => failure.message.clone(),
    }
}
