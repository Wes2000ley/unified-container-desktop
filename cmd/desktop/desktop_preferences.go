package main

// DesktopPreferences contains settings that apply to the current Windows user.
// They do not affect the machine-wide container services.
type DesktopPreferences struct {
	StartAtLogin   bool `json:"startAtLogin"`
	StartMinimized bool `json:"startMinimized"`
	CloseToTray    bool `json:"closeToTray"`
}

type desktopPreferencesStore interface {
	Get() DesktopPreferences
	Set(DesktopPreferences) error
}
