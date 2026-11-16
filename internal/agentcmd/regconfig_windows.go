//go:build windows

package agentcmd

import "golang.org/x/sys/windows/registry"

// registryConfig čita adresu konzole i token iz registry-ja (ključ upisuje
// MSI paket; čitaju ga samo SYSTEM i administratori).
func registryConfig(key string) (consoleURL, token string) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", ""
	}
	defer k.Close()
	consoleURL, _, _ = k.GetStringValue("ConsoleURL")
	token, _, _ = k.GetStringValue("Token")
	return consoleURL, token
}
