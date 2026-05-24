// A tiny fixture module for kotlin-language-server adapter integration tests.
package fixture

class Greeter(private val name: String) {
    fun greet(): String = "Hello, $name!"
}

fun makeGreeter(name: String): Greeter = Greeter(name)
