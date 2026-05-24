// A tiny fixture module for typescript-language-server adapter integration tests.

export class Greeter {
  constructor(private readonly name: string) {}

  greet(): string {
    return `Hello, ${this.name}!`;
  }
}

export function makeGreeter(name: string): Greeter {
  return new Greeter(name);
}
