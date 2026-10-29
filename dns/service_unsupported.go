//go:build !darwin && !linux

package dns

import "errors"

const defaultRouteIsMatchDomain = false

func StartService(ServiceConfig) (Service, error) {
	return nil, errors.New("tailmix MagicDNS OS integration is currently implemented only on Darwin and Linux")
}
