//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/windows/registry"
)

const (
	preferencesKeyPath = `Software\UnifiedContainerDesktop\Desktop`
	autoStartKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	autoStartValue     = "Unified Container Desktop"
)

type registryDesktopPreferencesStore struct {
	mu    sync.RWMutex
	value DesktopPreferences
}

func newDesktopPreferencesStore() (desktopPreferencesStore, error) {
	value, err := readDesktopPreferences()
	if err != nil {
		return nil, err
	}
	return &registryDesktopPreferencesStore{value: value}, nil
}

func (s *registryDesktopPreferencesStore) Get() DesktopPreferences {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

func (s *registryDesktopPreferencesStore) Set(next DesktopPreferences) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	oldRun, err := readAutoStartCommand()
	if err != nil {
		return fmt.Errorf("read current-user startup setting: %w", err)
	}
	nextRun := ""
	if next.StartAtLogin {
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate desktop executable for Windows startup: %w", err)
		}
		nextRun = startupCommand(executable)
	}
	changedRun := nextRun != oldRun
	if changedRun {
		if err := writeAutoStartCommand(nextRun); err != nil {
			return fmt.Errorf("update current-user Windows startup setting: %w", err)
		}
	}
	if err := writeStoredPreferences(next); err != nil {
		rollbackErr := writeStoredPreferences(s.value)
		if changedRun {
			rollbackErr = errors.Join(rollbackErr, writeAutoStartCommand(oldRun))
		}
		return errors.Join(fmt.Errorf("save current-user desktop preferences: %w", err), rollbackErr)
	}
	s.value = next
	return nil
}

func readDesktopPreferences() (DesktopPreferences, error) {
	result := DesktopPreferences{CloseToTray: true}
	key, err := registry.OpenKey(registry.CURRENT_USER, preferencesKeyPath, registry.QUERY_VALUE)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return result, fmt.Errorf("open current-user desktop preferences: %w", err)
	}
	if err == nil {
		defer key.Close()
		minimized, err := readBoolValue(key, "StartMinimized", false)
		if err != nil {
			return result, err
		}
		closeToTray, err := readBoolValue(key, "CloseToTray", true)
		if err != nil {
			return result, err
		}
		result.StartMinimized = minimized
		result.CloseToTray = closeToTray
	}
	command, err := readAutoStartCommand()
	if err != nil {
		return result, fmt.Errorf("read current-user Windows startup setting: %w", err)
	}
	result.StartAtLogin = command != ""
	return result, nil
}

func readBoolValue(key registry.Key, name string, fallback bool) (bool, error) {
	value, _, err := key.GetIntegerValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return fallback, nil
	}
	if err != nil {
		return false, fmt.Errorf("read desktop preference %s: %w", name, err)
	}
	if value > 1 {
		return false, fmt.Errorf("desktop preference %s has invalid value %d", name, value)
	}
	return value == 1, nil
}

func writeStoredPreferences(value DesktopPreferences) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, preferencesKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetDWordValue("StartMinimized", boolDWORD(value.StartMinimized)); err != nil {
		return err
	}
	return key.SetDWordValue("CloseToTray", boolDWORD(value.CloseToTray))
}

func boolDWORD(value bool) uint32 {
	if value {
		return 1
	}
	return 0
}

func readAutoStartCommand() (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, autoStartKeyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer key.Close()
	command, _, err := key.GetStringValue(autoStartValue)
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return command, err
}

func writeAutoStartCommand(command string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, autoStartKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if command == "" {
		err := key.DeleteValue(autoStartValue)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	return key.SetStringValue(autoStartValue, command)
}

func startupCommand(executable string) string {
	// Windows file names cannot contain quotes, so quoting the full path is enough.
	return `"` + executable + `" --background`
}
