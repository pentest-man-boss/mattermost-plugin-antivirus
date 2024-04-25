package relay

import (
	"io"
	"net"
)

func relay(src net.Conn, dst net.Conn, stop chan bool) {
	_, err := io.Copy(dst, src)
	if err != nil {
		return
	}
	dst.Close()
	src.Close()
	stop <- true
}

func StartRelay(src net.Conn, dst net.Conn) {
	stop := make(chan bool, 2)

	go relay(src, dst, stop)
	go relay(dst, src, stop)

	<-stop
	// select {
	// case <-stop:
	//	return
	//}
}
