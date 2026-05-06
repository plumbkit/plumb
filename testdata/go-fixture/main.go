package main

import "fmt"

// Greeter produces greeting messages.
type Greeter struct {
	Prefix string
}

// Greet returns a greeting for name.
func (g Greeter) Greet(name string) string {
	return fmt.Sprintf("%s, %s!", g.Prefix, name)
}

func main() {
	g := Greeter{Prefix: "Hello"}
	fmt.Println(g.Greet("world"))
}
