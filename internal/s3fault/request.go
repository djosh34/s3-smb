// SPDX-License-Identifier: AGPL-3.0-only

package s3fault

import (
	"io"
	"log"
	"net"
	"net/http"
	"sync/atomic"
)

type cutRequestBody struct {
	io.ReadCloser
	proxy     *Proxy
	request   *http.Request
	remaining int64
	cut       atomic.Bool
}

func (body *cutRequestBody) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if body.remaining == 0 {
		var probe [1]byte
		n, err := body.ReadCloser.Read(probe[:])
		if n != 0 {
			if body.cut.CompareAndSwap(false, true) {
				body.proxy.observe(body.request, "request-cut", 0)
			}
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if int64(len(dst)) > body.remaining {
		dst = dst[:body.remaining]
	}
	n, err := body.ReadCloser.Read(dst)
	body.remaining -= int64(n)
	return n, err
}

func resetConnection(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		log.Printf("s3 fault proxy: hijack cut upload: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetLinger(0); err != nil {
			log.Printf("s3 fault proxy: reset cut upload: %v", err)
		}
	}
	if err := conn.Close(); err != nil {
		log.Printf("s3 fault proxy: close cut upload: %v", err)
	}
}
