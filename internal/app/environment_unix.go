//go:build !windows

package app

import "errors"

var persistRegistry = func(scope, value string, dry bool) error {
	return errors.New("Windows environment registry is unavailable on this platform")
}
