package putils

import (
	"net"
	"strings"
)

// GetLocalIP 获取本地IP地址
func GetLocalIP() net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		panic(err)
	}
	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPNet); ok && !ip.IP.IsLoopback() {
			if ip.IP.To4() != nil {
				return ip.IP
			}
		}
	}
	return nil
}

// ErrContain 错误包含字符
func ErrContain(err error, s string) bool {
	return strings.Contains(err.Error(), s)
}
