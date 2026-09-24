//go:build !linux && !windows

package tun

// Create 未实现平台(M2 扩展)。
func Create(_ string) (Device, error) {
	return nil, ErrUnsupported
}
