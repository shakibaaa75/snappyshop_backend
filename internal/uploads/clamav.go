package uploads

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"
)

// Scanner checks raw upload bytes for malware before they are processed.
type Scanner interface {
	// Scan returns clean=false and the signature name when malware is found.
	Scan(ctx context.Context, data []byte) (clean bool, signature string, err error)
}

// ClamAV talks to a clamd daemon over TCP (INSTREAM). No dependency needed.
type ClamAV struct {
	Addr    string // host:port, e.g. "127.0.0.1:3310"
	Timeout time.Duration
}

func (c *ClamAV) Scan(ctx context.Context, data []byte) (bool, string, error) {
	to := c.Timeout
	if to <= 0 {
		to = 15 * time.Second
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return false, "", errors.New("clamav unreachable")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(to))
	if _, err := conn.Write([]byte("zINSTREAM\x00")); err != nil {
		return false, "", errors.New("clamav write failed")
	}
	const chunk = 64 << 10
	var hdr [4]byte
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		binary.BigEndian.PutUint32(hdr[:], uint32(end-off))
		if _, err := conn.Write(hdr[:]); err != nil {
			return false, "", errors.New("clamav write failed")
		}
		if _, err := conn.Write(data[off:end]); err != nil {
			return false, "", errors.New("clamav write failed")
		}
	}
	binary.BigEndian.PutUint32(hdr[:], 0)
	if _, err := conn.Write(hdr[:]); err != nil {
		return false, "", errors.New("clamav write failed")
	}
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	resp := strings.TrimRight(string(buf[:n]), "\x00\r\n ")
	switch {
	case strings.HasSuffix(resp, "OK"):
		return true, "", nil
	case strings.HasSuffix(resp, "FOUND"):
		sig := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(resp, "stream:"), "FOUND"))
		return false, sig, nil
	}
	return false, "", errors.New("clamav error response")
}
