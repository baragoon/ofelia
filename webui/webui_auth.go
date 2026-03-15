package webui

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/alexedwards/argon2id"
)

// WebUIPasswordEnv is the environment variable for the Argon2id hash of the UI password.
// This is not a secret, just the env var name (gosec false positive).
// #nosec G101 -- Not a credential, just an env var name
const WebUIPasswordEnv = "OFELIA_WEBUI_PASSWORD_HASH"

// SetWebUIPasswordHash sets the Argon2id hash in the environment (for tests or setup).
func SetWebUIPasswordHash(hash string) {
	if err := os.Setenv(WebUIPasswordEnv, hash); err != nil {
		  log.Printf("[ERROR] Failed to set environment variable: %v", err)
	}
}

// CheckWebUIPassword checks the password against the Argon2id hash in the environment.
func CheckWebUIPassword(user, password string) bool {
       hash := os.Getenv(WebUIPasswordEnv)
       if hash == "" {
	       // Secure: require password hash to be set in production
	       // Optionally log a warning here if you have a logger
	       return false
       }
       ok, err := argon2id.ComparePasswordAndHash(password, hash)
       if err != nil {
	       // Optionally log the error here if you have a logger
	       return false
       }
       return ok
}

// RequireWebUIAuth is a middleware that enforces HTTP Basic Auth with Argon2id password check.
func RequireWebUIAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !CheckWebUIPassword(user, pass) {
			   w.Header().Set("WWW-Authenticate", `Basic realm="Ofelia Web UI"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GenerateWebUIPasswordHash generates a strong Argon2id hash for a password.
func GenerateWebUIPasswordHash(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("password must be at least 12 characters")
	}
	return argon2id.CreateHash(password, argon2id.DefaultParams)
}
