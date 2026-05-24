/// A tiny fixture module for sourcekit-lsp adapter integration tests.

public struct Greeter {
    public let name: String

    public init(name: String) {
        self.name = name
    }

    public func greet() -> String {
        return "Hello, \(name)!"
    }
}

public func makeGreeter(name: String) -> Greeter {
    return Greeter(name: name)
}
