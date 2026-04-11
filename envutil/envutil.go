package envutil

import "os"

// GetDefault returns the value of the environment variable named by key,
// or def if the variable is empty or not set.
func GetDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
