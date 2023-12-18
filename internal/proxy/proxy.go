// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/smallnest/chanx"

	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
)

const ReadWriteDeadline = 3 * time.Second

type ProxyConfig struct {
	Endpoints []Endpoint `yaml:"endpoints"`
}

type Endpoint struct {
	Address string `yaml:"address"`
	Port    int32  `yaml:"port"`
}

type Proxy struct {
	mode          apiv1.PortProtocol
	listenAddress string
	listenPort    int32

	EffectiveAddress string
	EffectivePort    int32

	endpointConfigLoadedChannel *chanx.UnboundedChan[*ProxyConfig]

	lifetimeCtx context.Context
	log         logr.Logger
}

// Creates a reverse proxy instance.
// The proxy will listen on the specified address and port, and forward incoming connections to the endpoints specified in the config file.
// The proxy will reload the config file when it changes.
// The proxy will stop when the lifetime context is cancelled.
// If the address is empty, the proxy will listen on localhost. The effectiveAddress field will contain the actual listened-on IPv4 or IPv6 address.
// If the port is 0, the proxy will listen on a random port. The effectivePort field will contain the actual listened-on port.
func NewProxy(mode apiv1.PortProtocol, listenAddress string, listenPort int32, lifetimeCtx context.Context, log logr.Logger) *Proxy {
	return &Proxy{
		mode:          mode,
		listenAddress: listenAddress,
		listenPort:    listenPort,

		endpointConfigLoadedChannel: chanx.NewUnboundedChan[*ProxyConfig](lifetimeCtx, 1),

		lifetimeCtx: lifetimeCtx,
		log:         log,
	}
}

func (p *Proxy) Start() error {
	if p.listenAddress == "" {
		p.listenAddress = "localhost"
	}

	lc := net.ListenConfig{}
	if p.mode == apiv1.TCP {
		tcpListener, err := lc.Listen(p.lifetimeCtx, "tcp", fmt.Sprintf("%s:%d", p.listenAddress, p.listenPort))
		if err != nil {
			return err
		}

		p.EffectiveAddress = tcpListener.Addr().(*net.TCPAddr).IP.String()
		p.EffectivePort = int32(tcpListener.Addr().(*net.TCPAddr).Port)

		go p.runTCP(tcpListener)
	} else if p.mode == apiv1.UDP {
		udpListener, err := lc.ListenPacket(p.lifetimeCtx, "udp", fmt.Sprintf("%s:%d", p.listenAddress, p.listenPort))
		if err != nil {
			return err
		}

		p.EffectiveAddress = udpListener.LocalAddr().(*net.UDPAddr).IP.String()
		p.EffectivePort = int32(udpListener.LocalAddr().(*net.UDPAddr).Port)

		go p.runUDP(udpListener)
	} else {
		return fmt.Errorf("unsupported proxy mode: %s", p.mode)
	}

	return nil
}

func (p *Proxy) ConfigChanged(newConfig *ProxyConfig) {
	p.endpointConfigLoadedChannel.In <- newConfig
}

func (p *Proxy) stop(listener io.Closer) {
	// This Close call will stop TCP Accept / UDP ReadFrom calls, which will ultimately cause the runTCP / runUDP functions to exit
	if err := listener.Close(); err != nil {
		p.log.Error(err, "Error stopping proxy")
	}
}

func (p *Proxy) runTCP(tcpListener net.Listener) {
	defer p.stop(tcpListener)

	// Wait until the config has been loaded the first time before accepting any connections
	config := <-p.endpointConfigLoadedChannel.Out

	// Make a channel that will receive a connection when one is accepted
	connectionChannel := chanx.NewUnboundedChan[net.Conn](p.lifetimeCtx, 1)
	go func() {
		for {
			if p.lifetimeCtx.Err() != nil {
				return
			}

			// Accept will block until a connection is received or the listener is closed via p.stop()
			incoming, err := tcpListener.Accept()
			if errors.Is(err, net.ErrClosed) {
				// Normal shutdown pathway, don't log
			} else if err != nil {
				p.log.Info("Error accepting TCP connection: %s", err)
			} else {
				connectionChannel.In <- incoming
			}
		}
	}()

	for {
		select {
		case <-p.lifetimeCtx.Done():
			return
		case config = <-p.endpointConfigLoadedChannel.Out:
			if p.lifetimeCtx.Err() != nil {
				return
			}
			p.log.V(1).Info("Config file changed, reloading")
		case incoming := <-connectionChannel.Out:
			if p.lifetimeCtx.Err() != nil {
				return
			}
			go func() {
				if err := p.handleTCPConnection(incoming, config); err != nil {
					p.log.Info("Error handling TCP connection", err)
				}

				p.log.V(1).Info(fmt.Sprintf("Done handling TCP connection from %s", incoming.RemoteAddr().String()))
			}()
		}
	}
}

func (p *Proxy) handleTCPConnection(incoming net.Conn, config *ProxyConfig) error {
	defer incoming.Close()

	if p.lifetimeCtx.Err() != nil {
		return nil
	}

	endpoint, err := selectRandomEndpoint(config)
	if err != nil {
		return err
	}

	p.log.V(1).Info(fmt.Sprintf("Accepted TCP connection from %s, forwarding to %s:%d", incoming.RemoteAddr().String(), endpoint.Address, endpoint.Port))

	var d net.Dialer
	if outgoing, err := d.DialContext(p.lifetimeCtx, "tcp", fmt.Sprintf("%s:%d", endpoint.Address, endpoint.Port)); err != nil {
		return err
	} else {
		defer outgoing.Close()
		wg := &sync.WaitGroup{}
		wg.Add(2)
		go p.copyStream(incoming, outgoing, wg)
		go p.copyStream(outgoing, incoming, wg)
		wg.Wait()
	}

	return nil
}

func (p *Proxy) copyStream(incoming, outgoing net.Conn, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		if p.lifetimeCtx.Err() != nil {
			return
		}

		deadline := time.Now().Add(ReadWriteDeadline)

		if err := incoming.SetReadDeadline(deadline); err != nil {
			p.log.Info("Error setting read deadline", err)
			return
		}
		if err := outgoing.SetWriteDeadline(deadline); err != nil {
			p.log.Info("Error setting write deadline", err)
			return
		}

		// Copy will block for at most 3 seconds before hitting the deadline, at which point it will
		// return an error. This is expected so it won't be logged, and as long as the lifetimeCtx
		// is still active, the deadline will be refreshed.
		if bytesWritten, err := io.Copy(outgoing, incoming); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				// This is expected regularly, so don't log it
				continue
			} else {
				p.log.Info("Error copying stream", err)
				return
			}
		} else if bytesWritten == 0 {
			// Connection has closed normally
			return
		}
	}
}

func (p *Proxy) runUDP(udpListener net.PacketConn) {
	defer p.stop(udpListener)

	// Wait until the config file has been loaded the first time before accepting any packets
	config := <-p.endpointConfigLoadedChannel.Out
	buffer := make([]byte, 64*1024) // 64KiB is the max UDP packet size)

	for {
		select {
		case config = <-p.endpointConfigLoadedChannel.Out:
			if p.lifetimeCtx.Err() != nil {
				return
			}
			p.log.V(1).Info("Config file changed, reloading")
		default:
			// No config change, continue
		}

		if p.lifetimeCtx.Err() != nil {
			return
		}

		if err := udpListener.SetReadDeadline(time.Now().Add(ReadWriteDeadline)); err != nil {
			p.log.Info("Error setting read deadline", err)
			return
		}

		// ReadFrom will block for at most 3 seconds before hitting the deadline, at which point it will
		// return an error. This is expected so it won't be logged, and as long as the lifetimeCtx
		// is still active, the deadline will be refreshed.
		bytesRead, addr, err := udpListener.ReadFrom(buffer)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// This is expected regularly, don't log
			continue
		} else if err != nil {
			p.log.Info("Error reading UDP packet", err)
		} else {
			if err := p.handleUDPPacket(buffer[:bytesRead], addr, config); err != nil {
				p.log.Info("Error handling UDP packet", err)
			}

			p.log.V(1).Info(fmt.Sprintf("Done handling UDP packet from %s", addr.String()))
		}
	}
}

func (p *Proxy) handleUDPPacket(buffer []byte, addr net.Addr, config *ProxyConfig) error {
	if p.lifetimeCtx.Err() != nil {
		return nil
	}

	endpoint, err := selectRandomEndpoint(config)
	if err != nil {
		return err
	}

	p.log.V(1).Info(fmt.Sprintf("Accepted UDP packet from %s, forwarding to %s:%d", addr.String(), endpoint.Address, endpoint.Port))

	// TODO: consider recycling the UDP "connection" instead of creating a new one for each packet
	var d net.Dialer
	if outgoing, err := d.DialContext(p.lifetimeCtx, "udp", fmt.Sprintf("%s:%d", endpoint.Address, endpoint.Port)); err != nil {
		return err
	} else {
		defer outgoing.Close()

		if err := outgoing.SetWriteDeadline(time.Now().Add(ReadWriteDeadline)); err != nil {
			return err
		}

		// Write is going to block until it is finished writing or hits the deadline
		if _, err := outgoing.Write(buffer); err != nil {
			return err
		}
	}

	return nil
}

func selectRandomEndpoint(config *ProxyConfig) (*Endpoint, error) {
	// Select a random endpoint from the configured list
	if len(config.Endpoints) == 0 {
		return nil, errors.New("no endpoints configured")
	}

	return &config.Endpoints[rand.Intn(len(config.Endpoints))], nil
}
