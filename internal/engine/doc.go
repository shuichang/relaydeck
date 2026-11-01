// Package engine supervises an optional sing-box child process.
//
// The engine configuration is administrator-controlled input. Applying it is
// equivalent to granting the sing-box process the capabilities expressed by
// that JSON configuration. This package never passes configuration values to a
// shell and does not provide a sandbox around sing-box. The service should run
// with only the privileges and filesystem access that the configured engine
// requires.
package engine
