//go:build !linux && !windows

package client

import "errors"

func applyGlobalRoutes(_ *Client) (func(), error) {
	return func() {}, errors.New("global mode unsupported on this platform")
}

func applySmartRoutes(_ *Client) (func(), error) {
	return func() {}, errors.New("smart mode unsupported on this platform")
}
