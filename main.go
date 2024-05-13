package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"net"

	"encoding/binary"

	log "github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
)

type cfgAll struct {
	Relay    string       `json:"relay"`
	Uuid     string       `json:"uuid"`
	Channels []cfgChannel `json:"channels"`
}

type cfgChannel struct {
	Id  uint16  `json:"id"`
	Udp *cfgUdp `json:"udp"`
}

type cfgUdp struct {
	Bind   string `json:"bind"`
	Client string `json:"client"`
}

type Muxer struct {
	config *cfgAll
	outCh  map[uint16]chan []byte
	inCh   chan []byte
}

func loadConfig(fileName string) (*cfgAll, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var cfg = &cfgAll{}
	dec := json.NewDecoder(file)
	err = dec.Decode(cfg)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func NewMuxer(cfg *cfgAll) *Muxer {
	return &Muxer{
		config: cfg,
	}
}

func (m *Muxer) Serve() error {
	m.outCh = make(map[uint16]chan []byte, 16)
	m.inCh = make(chan []byte, 128)
	for _, ch := range m.config.Channels {
		if ch.Udp != nil {
			err := m.serveUdp(ch.Id, ch.Udp)
			if err != nil {
				return err
			}
		}
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		conn, _, err := websocket.Dial(ctx, m.config.Relay, nil)
		if err != nil {
			return fmt.Errorf("websocket.Dial: %w", err)
		}
		log.Info("websocket connected")
		conn.SetReadLimit(-1)
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err = conn.Write(ctx, websocket.MessageText, []byte(m.config.Uuid))
		if err != nil {
			return fmt.Errorf("websocket login: %w", err)
		}
		log.Info("websocket logged in")
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		wg := &sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case buf := <-m.inCh:
					err := conn.Write(ctx, websocket.MessageBinary, buf)
					if err != nil {
						log.Error(err)
						cancel()
						return
					}
				case <-ctx.Done():
					err := conn.CloseNow()
					if err != nil {
						log.Error(err)
					}
					return
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				typ, buf, err := conn.Read(ctx)
				if err != nil {
					log.Error(err)
					cancel()
					return
				}
				if len(buf) < 2 || typ != websocket.MessageBinary {
					continue
				}
				chNum := binary.BigEndian.Uint16(buf[:2])
				if ch, ok := m.outCh[chNum]; ok {
					ch <- buf[2:]
				}
			}
		}()
		log.Info("transmitting data")
		wg.Wait()
		err = conn.CloseNow()
		if err != nil {
			log.Error(err)
		}
		log.Info("lost websocket server, reconnect in 5s")
		time.Sleep(5 * time.Second)
	}

}

func (m *Muxer) serveUdp(chId uint16, cfg *cfgUdp) error {
	pc, err := net.ListenPacket("udp", cfg.Bind)
	if err != nil {
		return err
	}
	log.WithField("bind", pc.LocalAddr()).
		WithField("channel", chId).
		Info("UDP listening")
	clientAddr, err := net.ResolveUDPAddr("udp", cfg.Client)
	if err != nil {
		return err
	}
	m.outCh[chId] = make(chan []byte)
	go func() {
		for {
			buf := make([]byte, 65536)
			binary.BigEndian.PutUint16(buf[:2], chId)
			n, addr, err := pc.ReadFrom(buf[2:])
			uAddr := addr.(*net.UDPAddr)
			if clientAddr.IP == nil && clientAddr.Port == 0 {
				clientAddr = uAddr
				log.WithField("addr", clientAddr).Info("attaching client")
			} else {
				if !clientAddr.IP.Equal(uAddr.IP) ||
					clientAddr.Port != uAddr.Port ||
					clientAddr.Zone != uAddr.Zone {
					log.WithField("expected", clientAddr).
						WithField("actual", uAddr).
						Info("client mismatch")
					continue
				}
			}
			if err != nil {
				log.Fatal(err)
			}
			m.inCh <- append([]byte{}, buf[:2+n]...)
		}
	}()
	go func() {
		for {
			buf := <-m.outCh[chId]
			_, err := pc.WriteTo(buf, clientAddr)
			if err != nil {
				log.Fatal(err)
			}
		}
	}()
	return nil
}

var (
	configFile = flag.String("config", "./config.json",
		"configuration file")
)

func main() {
	flag.Parse()
	log.Info("starting")
	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	log.WithField("config", *configFile).Info("loaded config")
	log.Fatal(NewMuxer(cfg).Serve())
}
