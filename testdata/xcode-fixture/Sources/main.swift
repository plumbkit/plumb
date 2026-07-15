struct Greeter {
    func greeting(for name: String) -> String {
        "Hello, \(name)!"
    }
}

func makeGreeting(name: String) -> String {
    Greeter().greeting(for: name)
}

print(makeGreeting(name: "Plumb"))
