package tunwg

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/armon/go-proxyproto"
	"github.com/ntnj/tunwg/internal"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type Listener struct {
	inner net.Listener
	port  uint16
}

var _ net.Listener = (*Listener)(nil)

func (l *Listener) Accept() (net.Conn, error) {
	return l.inner.Accept()
}

func (l *Listener) Close() error {
	return l.inner.Close()
}

func (l *Listener) Addr() net.Addr {
	return l
}

func (l *Listener) Network() string {
	return "tunwg"
}

func (l *Listener) String() string {
	return internal.GetEncodedIPPort(netip.AddrPortFrom(internal.GetLocalWgIp(), l.port))
}

func addServerPeer() error {
	log.Println("tunwg: initiating handshake to server")
	key := internal.GetPublicKey()
	req := internal.AddPeerReq{
		Key: key[:],
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpResp, err := http.Post("https://"+internal.ApiDomain()+"/add", "application/json", bytes.NewReader(reqBytes))
	if err != nil {
		return err
	}
	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("error adding peer: %v", httpResp.Status)
	}
	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return err
	}
	resp := internal.AddPeerResp{}
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return err
	}
	servKey, err := wgtypes.NewKey(resp.Key)
	if err != nil {
		return err
	}
	return internal.WgSetIpc([]string{
		"replace_peers=true",
		"public_key=" + hex.EncodeToString(servKey[:]),
		"endpoint=" + resp.Endpoint,
		fmt.Sprintf("allowed_ip=%v/128", internal.GetIPForKey(servKey)),
		"persistent_keepalive_interval=25",
	})
}

func NewListener(name string) (net.Listener, error) {
	if err := internal.Initialize(); err != nil {
		return nil, err
	}
	h256 := sha256.Sum256([]byte(name))
	port := binary.LittleEndian.Uint16(h256[:2])

	l, err := internal.ListenTCPWg(&net.TCPAddr{Port: int(port)})
	if err != nil {
		return nil, err
	}
	if err := startListenersOnce(); err != nil {
		return nil, err
	}
	pl := &proxyproto.Listener{Listener: l, UnknownOK: true}
	tl := tls.NewListener(&Listener{pl, port}, internal.GetTLSConfig())
	log.Printf("tunwg: %v <= https://%v.%v", name, tl.Addr(), internal.ApiDomain())
	return tl, nil
}

var handshakeOnce sync.Once

func startListenersOnce() error {
	var err error
	handshakeOnce.Do(func() {
		if err = internal.Initialize(); err != nil {
			return
		}
		if err = addServerPeer(); err != nil {
			return
		}
		go backgroundMonitor()
	})
	return err
}

func backgroundMonitor() {
	for range time.Tick(30 * time.Second) {
		dev, err := internal.GetConnectedPeers()
		if err != nil {
			log.Printf("WARNING: internal error: %v", err)
		}
		if len(dev.Peers) != 1 {
			log.Printf("WARNING: internal error: incorrect len: %+v", *dev)
		}
		p := dev.Peers[0]
		if time.Since(p.LastHandshakeTime) > 150*time.Second {
			if err := addServerPeer(); err != nil {
				log.Printf("WARNING: Lost connection to server: %s", err)
			}
		}
	}
}
