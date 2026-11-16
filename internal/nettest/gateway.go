package nettest

import "errors"

// ErrNoGateway se vraća kada podrazumevani gateway nije pronađen.
var ErrNoGateway = errors.New("podrazumevani gateway nije pronađen")

// DefaultGateway vraća IPv4 adresu podrazumevanog gateway-a ili grešku.
func DefaultGateway() (string, error) { return defaultGateway() }
