//! A tiny fixture crate for rust-analyzer adapter integration tests.

pub struct Greeter {
    pub name: String,
}

impl Greeter {
    pub fn new(name: String) -> Self {
        Greeter { name }
    }

    pub fn greet(&self) -> String {
        format!("Hello, {}!", self.name)
    }
}

pub fn make_greeter(name: &str) -> Greeter {
    Greeter::new(name.to_string())
}
