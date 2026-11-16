//go:build !windows

package main

// The notification-area icon is a Windows-only feature.
func startTray(_ *App) func()          { return func() {} }
func showTrayNotification(_, _ string) {}
