package sample

import (
	"fmt"
	"strings"
)

// Greeter builds greetings.
type Greeter struct {
	Name string
}

// Greet returns a greeting for the receiver.
func (g *Greeter) Greet() string {
	return fmt.Sprintf("hello %s", g.Name)
}

// Shout upper-cases s.
func Shout(s string) string {
	return strings.ToUpper(s)
}

func helper(n int) int {
	return n * 2
}
