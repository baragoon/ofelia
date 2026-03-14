package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/alexedwards/argon2id"
	"golang.org/x/term"
)


















func main() {
	fmt.Print("Enter a strong password (min 12 chars): ")
	pwBytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		fmt.Println("Error reading password:", err)
		os.Exit(1)
	}
	pw := strings.TrimSpace(string(pwBytes))
	if len(pw) < 12 {
		fmt.Println("Password must be at least 12 characters.")
		os.Exit(1)
	}
	hash, err := argon2id.CreateHash(pw, argon2id.DefaultParams)
	if err != nil {
		fmt.Println("Error generating hash:", err)
		os.Exit(1)
	}
	fmt.Println("Argon2id hash:")
	fmt.Println(hash)
}