package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"net"

	"encoding/binary"

	log "github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
)

var (
	configFile = flag.String("config", "./config.json",
		"configuration file")
)

type cfgAll struct {
	Relay           string       `json:"relay"`
	Uuid            string       `json:"uuid"`
	PingIntervalSec int          `json:"ping_interval_sec"`
	PingTimeoutSec  int          `json:"ping_timeout_sec"`
	Channels        []cfgChannel `json:"channels"`
}

type cfgChannel struct {
	Id  uint16  `json:"id"`
	Udp *cfgUdp `json:"udp"`
}

type cfgUdp struct {
	Bind   string `json:"bind"`
	Client string `json:"client"`
}

func loadConfig(fileName string) (*cfgAll, error) {
	data, err := os.ReadFile(fileName)
	if err != nil {
		return nil, err
	}
	var cfg = &cfgAll{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func isCompleteUdpAddr(addr *net.UDPAddr) bool {
	return !addr.IP.IsUnspecified() && (addr.Port != 0)
}

func isMatchingAddr(spec, addr *net.UDPAddr) bool {
	if !spec.IP.IsUnspecified() && !spec.IP.Equal(addr.IP) {
		return false
	}
	if spec.Port != 0 && spec.Port != addr.Port {
		return false
	}
	return true
}

func readTofu(pc net.PacketConn, buf []byte) (int, *net.UDPAddr, error) {
	if _, ok := pc.LocalAddr().(*net.UDPAddr); !ok {
		return 0, nil, errors.New("invalid non-udp packetConn")
	}
	n, addr, err := pc.ReadFrom(buf)
	if err != nil {
		return 0, nil, err
	}
	return n, addr.(*net.UDPAddr), nil
}

type packetSink chan<- []byte

type muxer struct {
	sm     sync.Map // id to packetSink
	config *cfgAll
}

func (m *muxer) serveUdp(ctx0 context.Context, pc net.PacketConn, vchid uint16, clnt *net.UDPAddr) error {
	ctx, cancel := context.WithCancelCause(ctx0)
	defer cancel(nil)

	prefix := make([]byte, binary.Size(vchid))
	binary.BigEndian.PutUint16(prefix, vchid)

	buf := make([]byte, 65536)

	if !isCompleteUdpAddr(clnt) {
		n, addr, err := readTofu(pc, buf)
		if err != nil {
			return err
		}
		m.gotTaggedPacket(append(prefix, buf[:n]...))
		clnt = addr
	}
	// register as sink
	sink := make(chan []byte, 16)
	m.sm.Store(vchid, packetSink(sink))
	defer m.sm.Delete(vchid)

	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel(nil)
		for {
			select {
			case packet := <-sink:
				_, err := pc.WriteTo(packet, clnt)
				if err != nil {
					cancel(err)
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel(nil)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				cancel(err)
				return
			}
			if !isMatchingAddr(clnt, addr.(*net.UDPAddr)) {
				continue
			}
			m.gotTaggedPacket(append(prefix, buf[:n]...))
		}
	}()
	return context.Cause(ctx)
}

func (m *muxer) dialWS(ctx0 context.Context, u, uuid string) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx0, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx0, u, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(65538)
	err = conn.Write(ctx, websocket.MessageText, []byte(uuid))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (m *muxer) pollWS(ctx0 context.Context, u, uuid string) error {
	log := log.WithField("ws", u)
	ctx, cancel := context.WithCancelCause(ctx0)
	defer cancel(nil)

	conn, err := m.dialWS(ctx, u, uuid)
	if err != nil {
		cancel(err)
		return err
	}
	log.Info("connected to ", u)
	defer conn.CloseNow()

	var wg sync.WaitGroup
	my := make(chan []byte, 16)
	m.sm.Store("ws", packetSink(my))

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel(nil)
		for {
			select {
			case packet := <-my:
				err := conn.Write(ctx, websocket.MessageBinary, packet)
				if err != nil {
					cancel(err)
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	if m.config.PingIntervalSec != 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(time.Second * time.Duration(m.config.PingIntervalSec))
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					func() {
						ctx1, cancel1 := context.WithTimeout(ctx,
							time.Second*time.Duration(m.config.PingTimeoutSec))
						defer cancel1()
						started := time.Now()
						err := conn.Ping(ctx1)
						if err != nil {
							cancel(err)
							return
						}
						duration := time.Now().Sub(started)
						log.WithField("rtt", duration).Info("WS ping-pong")
					}()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel(nil)
		for {
			typ, msg, err := conn.Read(ctx)
			if err != nil {
				cancel(err)
				return
			}
			if typ != websocket.MessageBinary {
				continue
			}
			if len(msg) < 2 {
				continue
			}
			vchid := binary.BigEndian.Uint16(msg)
			sink, ok := m.sm.Load(vchid)
			if ok {
				select {
				case <-ctx.Done():
					return
				case sink.(packetSink) <- msg[2:]:
				default:
				}
			}
		}
	}()
	wg.Wait()
	return context.Cause(ctx)
}

func (m *muxer) gotTaggedPacket(packet []byte) {
	val, ok := m.sm.Load("ws")
	if !ok {
		// drop packet while there is no ws connection
		return
	}
	sink := val.(packetSink)
	select {
	case sink <- packet:
	default:
	}
}

func Serve(ctx0 context.Context, configFileName string) error {
	lc := net.ListenConfig{}
	ctx, cancel := context.WithCancelCause(ctx0)
	defer cancel(nil)
	cfg, err := loadConfig(*configFile)
	if err != nil {
		return err
	}
	log.WithField("config", *configFile).Info("loaded config")

	m := &muxer{
		config: cfg,
	}
	var wg sync.WaitGroup

	for _, cfgch := range cfg.Channels {
		clientAddr, err := net.ResolveUDPAddr("udp",
			cfgch.Udp.Client)

		if err != nil {
			return err
		}
		addr := cfgch.Udp.Bind
		log := log.WithField("addr", addr)
		if err != nil {
			return err
		}

		log.Info("listening")
		ln, err := lc.ListenPacket(ctx, "udp", addr)
		if err != nil {
			return err
		}
		if ua, ok := ln.LocalAddr().(*net.UDPAddr); ok {
			log.Info("Bound to ", ua.String())
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ctx.Done()
			ln.Close()
		}()

		wg.Add(1)
		vchid := cfgch.Id
		go func() {
			defer wg.Done()
			err := m.serveUdp(ctx, ln, vchid, clientAddr)
			if err != nil {
				cancel(err)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		log := log.WithField("ws", cfg.Relay)
		for {
			err := m.pollWS(ctx, cfg.Relay, cfg.Uuid)
			if err != nil {
				log.Warn(err)
			}

			tm := time.NewTimer(5 * time.Second)
			select {
			case <-tm.C:
			case <-ctx.Done():
				return
			}
			log.Info("ws reconnect")
		}
	}()
	wg.Wait()
	return context.Cause(ctx)
}

type sigErr struct {
	sig os.Signal
}

func (s *sigErr) Error() string {
	return fmt.Sprintf("Quit: %v", s.sig)
}

func main() {
	flag.Parse()
	log.Info("starting")

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigc
		cancel(&sigErr{sig})
	}()
	err := Serve(ctx, *configFile)
	if _, ok := err.(*sigErr); !ok {
		log.Fatal(err)
	}
}
