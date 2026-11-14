package api

import "strings"

// IsEngineMethod identifies operations implemented by one Docker backend.
// The backend validates the concrete method and its parameters. Authorization
// remains the controller's responsibility before forwarding any request.
func IsEngineMethod(method string) bool {
	if method == "engine.status" || method == "engine.configure" {
		return true
	}
	for _, prefix := range []string{"container.", "image.", "volume.", "network.", "storage.", "registry.", "transfer."} {
		if strings.HasPrefix(method, prefix) {
			return true
		}
	}
	return false
}
