package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"os"
	"strings"
)

// defaultGateway is the IPv4 default route's gateway, from /proc/net/route,
// or "" when there is none.
func defaultGateway() string {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer file.Close()
	return parseDefaultGateway(file)
}

// parseDefaultGateway reads /proc/net/route's format: a header, then one
// route per line, destination and gateway as little-endian hex.
func parseDefaultGateway(routes io.Reader) string {
	scanner := bufio.NewScanner(routes)
	scanner.Scan() // the header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, binary.LittleEndian.Uint32(raw))
		if ip.IsUnspecified() {
			continue
		}
		return ip.String()
	}
	return ""
}
