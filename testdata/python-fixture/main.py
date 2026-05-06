"""Minimal Python project used by pyright unit tests (mocked transport)."""


class Greeter:
    """Produces greeting messages."""

    def __init__(self, prefix: str) -> None:
        self.prefix = prefix

    def greet(self, name: str) -> str:
        """Return a greeting for name."""
        return f"{self.prefix}, {name}!"


if __name__ == "__main__":
    g = Greeter("Hello")
    print(g.greet("world"))
