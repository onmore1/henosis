package agent

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
)

// A session code is everything a friend needs to connect: where the host's
// computer can be reached and the secret that lets them in. It looks like
// "HEN" followed by letters and digits and is meant to be copied and pasted.

var codeAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

const codePrefix = "HEN"

// EncodeCode builds a session code. ips are IPv4 addresses of the host; extra
// is an optional address typed by the host (a public IP or a domain, with or
// without a port); secret is 16 bytes as hex.
func EncodeCode(ips []string, port int, extra, secret string) (string, error) {
	raw, err := hex.DecodeString(secret)
	if err != nil || len(raw) != 16 {
		return "", errors.New("the session secret is damaged")
	}
	if len(ips) > 12 {
		ips = ips[:12]
	}
	if len(extra) > 200 {
		return "", errors.New("the extra address is too long")
	}
	data := []byte{1}
	data = binary.BigEndian.AppendUint16(data, uint16(port))
	var packed []byte
	count := 0
	for _, ip := range ips {
		if v4 := net.ParseIP(ip).To4(); v4 != nil {
			packed = append(packed, v4...)
			count++
		}
	}
	data = append(data, byte(count))
	data = append(data, packed...)
	data = append(data, raw...)
	data = append(data, byte(len(extra)))
	data = append(data, extra...)
	sum := sha256.Sum256(data)
	data = append(data, sum[0], sum[1])
	return codePrefix + codeAlphabet.EncodeToString(data), nil
}

// DecodeCode reads a session code. It returns the addresses to try, in
// order, as "http://host:port", and the secret.
func DecodeCode(code string) (addresses []string, secret string, err error) {
	clean := strings.ToUpper(strings.Join(strings.Fields(code), ""))
	invalid := errors.New("that does not look like a session code; copy it again from your friend's Henosis window")
	if !strings.HasPrefix(clean, codePrefix) {
		return nil, "", invalid
	}
	data, decodeErr := codeAlphabet.DecodeString(strings.TrimPrefix(clean, codePrefix))
	if decodeErr != nil || len(data) < 1+2+1+16+1+2 || data[0] != 1 {
		return nil, "", invalid
	}
	body, check := data[:len(data)-2], data[len(data)-2:]
	if sum := sha256.Sum256(body); sum[0] != check[0] || sum[1] != check[1] {
		return nil, "", errors.New("the session code is incomplete or mistyped; copy it again")
	}
	port := int(binary.BigEndian.Uint16(body[1:3]))
	count := int(body[3])
	rest := body[4:]
	if len(rest) < count*4+16+1 {
		return nil, "", invalid
	}
	for i := 0; i < count; i++ {
		ip := net.IP(rest[i*4 : i*4+4]).String()
		addresses = append(addresses, "http://"+ip+":"+strconv.Itoa(port))
	}
	rest = rest[count*4:]
	secret = hex.EncodeToString(rest[:16])
	rest = rest[16:]
	if n := int(rest[0]); n > 0 {
		if len(rest) < 1+n {
			return nil, "", invalid
		}
		// What the host typed by hand goes first: they added it for a reason.
		addresses = append([]string{extraURL(string(rest[1:1+n]), port)}, addresses...)
	}
	if len(addresses) == 0 {
		return nil, "", errors.New("the session code contains no address")
	}
	return addresses, secret, nil
}

// extraURL turns "example.com", "1.2.3.4:9000" or "https://x" into a URL.
func extraURL(extra string, port int) string {
	extra = strings.TrimRight(strings.TrimSpace(extra), "/")
	scheme := "http://"
	for _, s := range []string{"http://", "https://"} {
		if strings.HasPrefix(strings.ToLower(extra), s) {
			scheme, extra = s, extra[len(s):]
		}
	}
	if _, _, err := net.SplitHostPort(extra); err != nil && scheme == "http://" {
		extra = net.JoinHostPort(extra, strconv.Itoa(port))
	}
	return scheme + extra
}
