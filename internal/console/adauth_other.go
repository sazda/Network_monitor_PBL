//go:build !windows

package console

func domainLogon(user, domain, pass string) error { return errADUnsupported }
