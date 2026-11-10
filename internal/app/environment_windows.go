package app

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var persistRegistry = writeRegistry

func writeRegistry(scope, value string, dry bool) error {
	if dry {
		return nil
	}
	root, subkey := registry.CURRENT_USER, `Environment`
	if scope == "system" {
		root, subkey = registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	}
	key, _, err := registry.CreateKey(root, subkey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open %s environment registry key (system scope requires elevation): %w", scope, err)
	}
	defer key.Close()
	if err := key.SetStringValue(cacheVariable, value); err != nil {
		return fmt.Errorf("set %s environment: %w", scope, err)
	}
	name, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return err
	}
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	result, _, callErr := proc.Call(0xffff, 0x001a, 0, uintptr(unsafe.Pointer(name)), 0x0002, 5000, 0)
	if result == 0 {
		return fmt.Errorf("environment saved but change broadcast failed; sign out and back in: %w", callErr)
	}
	return nil
}
