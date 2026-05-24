//! A tiny fixture module for zls adapter integration tests.

const std = @import("std");

pub const Greeter = struct {
    name: []const u8,

    pub fn greet(self: Greeter) []const u8 {
        return self.name;
    }
};

pub fn makeGreeter(name: []const u8) Greeter {
    return Greeter{ .name = name };
}
