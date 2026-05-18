package com.example;

/** Minimal Java class used by plumb integration tests. No external dependencies. */
public class Greeter {
    private final String prefix;

    public Greeter(String prefix) {
        this.prefix = prefix;
    }

    public String greet(String name) {
        return prefix + ", " + name + "!";
    }

    public static void main(String[] args) {
        Greeter g = new Greeter("Hello");
        System.out.println(g.greet("world"));
    }
}
