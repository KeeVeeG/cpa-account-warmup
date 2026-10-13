// Command cpa-account-warmup-helper serves the native plugin over a private socket.
package main

import (
	"net"
	"os"

	"cpa-account-warmup/internal/plugin"
)

func main() {
	file := os.NewFile(3, "plugin-ipc")
	if file == nil {
		os.Exit(1)
	}
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		os.Exit(1)
	}
	peer := newPeer(conn)
	peer.handler = plugin.New(peer)
	if peer.serve() != nil {
		os.Exit(1)
	}
}
