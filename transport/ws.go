// Package transport provides WebSocket tunneling functionality.
package transport

import (
	"bepass/dialer"
	"bepass/logger"
	"bepass/wsconnadapter"
	"context"
	"encoding/binary"
	"net"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

// EstablishedTunnel represents an established tunnel.
type EstablishedTunnel struct {
	tunnelWriteChannel chan UDPPacket
	bindWriteChannels  map[uint16]chan UDPPacket
	channelIndex       uint16
}

// WSTunnel represents a WebSocket tunnel.
type WSTunnel struct {
	BindAddress        string
	Dialer             *dialer.Dialer
	ReadTimeout        int
	WriteTimeout       int
	LinkIdleTimeout    int64
	EstablishedTunnels map[string]*EstablishedTunnel
	ShortClientID      string
}

// socks5TCPDial dials using SOCKS5 proxy.
func (w *WSTunnel) socks5TCPDial(_ context.Context, network, addr string) (net.Conn, error) {
	d, err := proxy.SOCKS5("tcp", w.BindAddress, nil, proxy.Direct)
	if err != nil {
		return nil, err
	}
	return d.Dial(network, addr)
}

// Dial establishes a WebSocket connection.
func (w *WSTunnel) Dial(endpoint string) (*websocket.Conn, error) {
	d := websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return w.socks5TCPDial(ctx, network, addr)
		},

		NetDialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return w.Dialer.TLSDial(func(network, addr, hostPort string) (net.Conn, error) {
				return w.socks5TCPDial(ctx, network, addr)
			}, network, addr, "")
		},
	}
	conn, _, err := d.Dial(endpoint, nil)
	return conn, err
}

// PersistentDial establishes a persistent WebSocket connection.
func (w *WSTunnel) PersistentDial(tunnelEndpoint string, bindWriteChannel chan UDPPacket) (chan UDPPacket, uint16, error) {
	if tunnel, ok := w.EstablishedTunnels[tunnelEndpoint]; ok {
		tunnel.channelIndex = tunnel.channelIndex + 1
		tunnel.bindWriteChannels[tunnel.channelIndex] = bindWriteChannel
		return tunnel.tunnelWriteChannel, tunnel.channelIndex, nil
	}

	tunnelWriteChannel := make(chan UDPPacket)

	w.EstablishedTunnels[tunnelEndpoint] = &EstablishedTunnel{
		tunnelWriteChannel: tunnelWriteChannel,
		bindWriteChannels:  make(map[uint16]chan UDPPacket),
		channelIndex:       1,
	}

	w.EstablishedTunnels[tunnelEndpoint].bindWriteChannels[1] = bindWriteChannel

	lastActivityStamp := time.Now().Unix()

	go func() {
		defer delete(w.EstablishedTunnels, tunnelEndpoint)
		if time.Now().Unix()-lastActivityStamp > w.LinkIdleTimeout {
			return
		}
		for {
			done := make(chan struct{})
			doneR := make(chan struct{})

			logger.Infof("connecting to %s\r\n", tunnelEndpoint)

			c, err := w.Dial(tunnelEndpoint)
			conn := wsconnadapter.New(c)

			if err != nil {
				logger.Errorf("error dialing udp over tcp tunnel: %v\r\n", err)
				continue
			}
			// Write
			go func() {
				defer func() {
					close(doneR)
					_ = conn.Close()
				}()

				defer logger.Info("write closed")

				for {
					select {
					case <-done:
						return
					case rt := <-tunnelWriteChannel:
						err := conn.SetWriteDeadline(time.Now().Add(time.Duration(w.WriteTimeout) * time.Second))
						if err != nil {
							return
						}

						bs := make([]byte, 2)
						binary.BigEndian.PutUint16(bs, rt.Channel)

						_, err = conn.Write(append([]byte(w.ShortClientID), append(bs, rt.Data...)...))
						if err != nil {
							logger.Info("write:", err)
							return
						}
						lastActivityStamp = time.Now().Unix()
					}
				}
			}()

			// Read
			func() {
				defer func() {
					close(done)
					_ = conn.Close()
				}()

				err := conn.SetReadDeadline(time.Now().Add(time.Duration(w.ReadTimeout) * time.Second))
				if err != nil {
					return
				}
				defer logger.Info("read closed")
				for {
					select {
					case <-doneR:
						return

					default:
						// 1- unpack the message
						// 2- find the channel that the message should write on
						// 3- write the message on that channel
						rawPacket := make([]byte, 32*1024)
						n, err := conn.Read(rawPacket)
						if n < 2 && err == nil {
							continue
						}

						if err != nil {
							if strings.Contains(err.Error(), "websocket: close") ||
								strings.Contains(err.Error(), "i/o") {
								logger.Errorf("reading from udp over tcp error: %v\r\n", err)
								return
							}
							logger.Errorf("reading from udp over TCP tunnel packet size error: %v\r\n", err)
							continue
						}

						// The first 2 packets of response are channel ID
						channelID := binary.BigEndian.Uint16(rawPacket[:2])

						pkt := UDPPacket{
							channelID,
							rawPacket[2:n],
						}

						if udpBindWriteChan, ok := w.EstablishedTunnels[tunnelEndpoint].bindWriteChannels[pkt.Channel]; ok {
							udpBindWriteChan <- pkt
							lastActivityStamp = time.Now().Unix()
						}
					}
				}
			}()
		}
	}()

	return tunnelWriteChannel, 1, nil
}
