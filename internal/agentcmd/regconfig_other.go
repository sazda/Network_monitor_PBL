//go:build !windows

package agentcmd

func registryConfig(string) (consoleURL, token string) { return "", "" }
