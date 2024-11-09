package ser2net

// Telnet Com Port Control Option - https://tools.ietf.org/html/rfc2217

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/PatrickRudolph/telnet"
	"go.bug.st/serial"
)

const (
	COMPORT byte = 0x2C
	SERVER  byte = 100
)
const (
	SIGNATURE byte = iota
	BAUDRATE
	DATASIZE
	PARITY
	STOPSIZE
	CONTROL
)

// Handler2217 negotiates Com Port for a specific connection.

type Handler2217 struct {
	old    map[string]serial.Mode
	enable map[string]bool
	worker *SerialWorker
	client bool
	sync.Mutex
}

func NewHandler2217(w *SerialWorker) *Handler2217 {
	return &Handler2217{
		old:    make(map[string]serial.Mode),
		enable: make(map[string]bool),
		worker: w,
	}
}

// Возвращает m e и создаёт их
func (h *Handler2217) me(c *telnet.Connection) (m serial.Mode, e bool) {
	s := c.RemoteAddr().String()
	e, ok := h.enable[s]
	if ok {
		m = h.old[s]
		return
	}
	log.Printf("-----------%s", s)
	e = false
	h.enable[s] = e

	m = serial.Mode{}
	h.old[s] = m
	return
}
func (h *Handler2217) meSet(c *telnet.Connection, m serial.Mode, e bool) {
	s := c.RemoteAddr().String()
	h.enable[s] = e
	h.old[s] = m
}

func (h *Handler2217) meDel(c *telnet.Connection) {
	s := c.RemoteAddr().String()
	delete(h.old, s)
	delete(h.enable, s)
}

// Server2217 enables Com Port negotiation on a Server.
func (h *Handler2217) Server2217(c *telnet.Connection) telnet.Negotiator {
	m, e := h.me(c)
	log.Printf("Server2217 %s->%s %v %v\r\n", c.LocalAddr(), c.RemoteAddr(), m, e)
	go func() {
		defer func() {
			h.meDel(c)
			log.Printf("Server2217 %s->%s %s\r\n", c.LocalAddr(), c.RemoteAddr(), "done")
		}()
		// Мониторим изменения на сервере режима последовательной консоли
		i := 0 // Времянка
		for {
			if h == nil {
				return
			}
			select {
			case <-h.worker.context.Done():
				h.IAC(c, telnet.DONT, h.OptionCode())
				return
			case <-time.After(time.Second):
				i++
				if i > 30 {
					if h.Controll(c) != nil {
						return
					}
					i = 0
				}
				h.setMode(c, nil)
			}
		}
	}()
	return h
}

// Client2217 enables Com Port negotiation on a Client.
func (h *Handler2217) Client2217(c *telnet.Connection) telnet.Negotiator {
	// log.Println("Client2217")
	h.client = true
	m, e := h.me(c)
	log.Printf("Client2217 %s->%s %v %v\r\n", c.LocalAddr(), c.RemoteAddr(), m, e)
	return h
}

// OptionCode returns the IAC code for Com Port.
func (h *Handler2217) OptionCode() byte {
	return COMPORT
}

// Пишет IAC и bs если без ошибки то enable=true
func (h *Handler2217) IAC(c *telnet.Connection, bs ...byte) (err error) {
	b := new(bytes.Buffer)
	b.WriteByte(telnet.IAC)
	b.Write(bs)
	_, err = c.Conn.Write(b.Bytes())
	log.Printf("%s->%s IAC %v %v\r\n", c.LocalAddr(), c.RemoteAddr(), bs, err)

	m, _ := h.me(c)
	h.meSet(c, m, err == nil)
	return
}

// Сервер пассивен
func (h *Handler2217) Offer(c *telnet.Connection) {
	if !h.client {
		// Сервер не инициирует обмен
		return
	}
	h.IAC(c, telnet.WILL, h.OptionCode())
}

// Сервер ответит если к нему подключена последовательная консоль
func (h *Handler2217) HandleWill(c *telnet.Connection) {
	if h.client || len(h.worker.args) > 0 {
		// Если клиент или интерпретатор команд или команда то не DO
		return
	}
	h.IAC(c, telnet.DO, h.OptionCode())
	h.Signature(c)
}

// Заменяем IAC на IAC IAC в writeSignature и writeBaudRate
func escapeIAC(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte{telnet.IAC}, []byte{telnet.IAC, telnet.IAC})
}

// Немного о себе
func (h *Handler2217) Signature(c *telnet.Connection) (err error) {
	m, e := h.me(c)
	if !e {
		return
	}
	payload := new(bytes.Buffer)
	subopt := SIGNATURE
	s := h.worker.path
	if h.client {
		s = c.LocalAddr().String()
	} else {
		subopt += SERVER
	}
	payload.WriteString(s)

	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt})
	b.Write(escapeIAC(payload.Bytes()))
	b.Write([]byte{telnet.IAC, telnet.SE})
	_, err = c.Conn.Write(b.Bytes())
	log.Printf("%s Signature %s to %s %v\r\n", c.LocalAddr(), s, c.RemoteAddr(), err)
	if err != nil {
		h.meSet(c, m, false)
	}
	return
}

// HandleDo send mode to server
func (h *Handler2217) HandleDo(c *telnet.Connection) {
	if !h.client {
		// Сервер не инициирует обмен
		return
	}
	m, _ := h.me(c)
	h.meSet(c, m, true)
	h.BaudRate(c)
	h.DataBits(c)
	h.Parity(c)
	h.StopBits(c)
	h.Controll(c)
}

func (h *Handler2217) BaudRate(c *telnet.Connection) (err error) {
	m, e := h.me(c)
	if !e {
		return
	}

	subopt := BAUDRATE
	v := uint32(m.BaudRate)
	if h.client {
		if m.InitialStatusBits != nil {
			err = h.Signature(c)
			if err != nil {
				return
			}
		} else {
			v = 0
		}
	} else {
		subopt += SERVER
	}
	b := new(bytes.Buffer)
	b.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt})
	// payload := make([]byte, 4)
	// binary.BigEndian.PutUint32(payload, uint32(h.will.BaudRate))
	payload := new(bytes.Buffer)
	binary.Write(payload, binary.BigEndian, v)
	b.Write(escapeIAC(payload.Bytes()))

	b.Write([]byte{telnet.IAC, telnet.SE})
	_, err = c.Conn.Write(b.Bytes())
	log.Printf("%s BaudRate %d to %s %v\r\n", c.LocalAddr(), v, c.RemoteAddr(), err)
	if err != nil {
		h.meSet(c, m, false)
	}
	return
}
func (h *Handler2217) DataBits(c *telnet.Connection) (err error) {
	m, e := h.me(c)
	if !e {
		return
	}
	subopt := DATASIZE
	v := byte(m.DataBits)
	if h.client {
		if m.InitialStatusBits != nil {
			err = h.Signature(c)
			if err != nil {
				return
			}
		} else {
			v = 0
		}
	} else {
		subopt += SERVER
	}
	_, err = c.Conn.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt,
		v,
		telnet.IAC, telnet.SE})
	// log.Printf("%s DataBits %d to %s %v\r\n", c.LocalAddr(), val, c.RemoteAddr(), err)
	if err != nil {
		h.meSet(c, m, false)
	}
	return
}
func (h *Handler2217) Parity(c *telnet.Connection) (err error) {
	m, e := h.me(c)
	if !e {
		return
	}
	subopt := PARITY
	v := byte(0)
	switch m.Parity {
	case serial.NoParity:
		v = 1
	case serial.OddParity:
		v = 2
	case serial.EvenParity:
		v = 3
	case serial.MarkParity:
		v = 4
	case serial.SpaceParity:
		v = 5
	}
	if h.client {
		if m.InitialStatusBits != nil {
			err = h.Signature(c)
			if err != nil {
				return
			}
		} else {
			v = 0
		}
	} else {
		subopt += SERVER
	}
	_, err = c.Conn.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt,
		v,
		telnet.IAC, telnet.SE})
	// log.Printf("%s Parity %d to %s %v\r\n", c.LocalAddr(), val, c.RemoteAddr(), err)
	if err != nil {
		h.meSet(c, m, false)
	}
	return
}
func (h *Handler2217) StopBits(c *telnet.Connection) (err error) {
	m, e := h.me(c)
	if !e {
		return
	}
	subopt := STOPSIZE
	v := byte(0)
	switch m.StopBits {
	case serial.OneStopBit:
		v = 1
	case serial.TwoStopBits:
		v = 2
	case serial.OnePointFiveStopBits:
		v = 3
	}
	if h.client {
		if m.InitialStatusBits != nil {
			err = h.Signature(c)
			if err != nil {
				return
			}
		} else {
			v = 0
		}
	} else {
		subopt += SERVER
	}
	c.Conn.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt,
		v,
		telnet.IAC, telnet.SE})
	// log.Printf("%s StopBits %d to %s %v\r\n", c.LocalAddr(), val, c.RemoteAddr(), err)
	if err != nil {
		h.meSet(c, m, false)
	}
	return
}

// Принудительно flowControl=N и единожды writeSignature для клиента.
// Используем для проверки живости.
func (h *Handler2217) Controll(c *telnet.Connection) (err error) {
	m, e := h.me(c)
	if !e {
		return
	}
	subopt := CONTROL
	if h.client {
		if m.InitialStatusBits == nil {
			m.InitialStatusBits = &serial.ModemOutputBits{}
			h.meSet(c, m, e)
			err = h.Signature(c)
			if err != nil {
				return
			}
		}
	} else {
		subopt += SERVER
	}
	v := byte(1) // N
	_, err = c.Conn.Write([]byte{telnet.IAC, telnet.SB, h.OptionCode(), subopt,
		v,
		telnet.IAC, telnet.SE})
	// log.Printf("%s Controll %d to %s %v\r\n", c.LocalAddr(), v, c.RemoteAddr(), err)
	if err != nil {
		h.meSet(c, m, false)
	}
	return
}

// HandleSB processes the information about Com Port sent from the client to the server and back.
// Вызывается из read
func (h *Handler2217) HandleSB(c *telnet.Connection, b []byte) {
	m, e := h.me(c)
	if !e {
		return
	}
	info := fmt.Sprintf("%s->%s IAC SB %v", c.RemoteAddr(), c.LocalAddr(), b)

	subopt := b[0]
	v := b[1]

	switch subopt {
	case SIGNATURE, SIGNATURE + SERVER:
		s := string(b[1:])
		if h.client {
			if s == c.LocalAddr().String() {
				s = "RouterOS"
			}
			if h.worker.url == "" {
				if h.worker.url == s {
					// return
				}
				h.worker.url = s
			}
		} else {
			if h.worker.remote == s {
				// return
			}
			// У сервера клиентов много.
			// h.Lock()
			h.worker.remote = s
			// h.Unlock()
		}

		info += " signature: " + s
		log.Print(info, "\r\n")
	case BAUDRATE, BAUDRATE + SERVER:
		m.BaudRate = int(binary.BigEndian.Uint32(b[1:]))
		if h.worker.mode.BaudRate == m.BaudRate {
			// return
		}
		info += " baudRate: "
		if m.BaudRate > 0 {
			info += fmt.Sprintf("%d", m.BaudRate)
		} else {
			info += "?"
		}
		log.Print(info, "\r\n")
		if h.client {
			h.worker.mode.BaudRate = m.BaudRate
			return
		}
		if m.BaudRate > 0 {
			h.worker.SetMode(&m)
		} else {
			h.meSet(c, h.worker.mode, e)
		}
		h.BaudRate(c)
	case DATASIZE, DATASIZE + SERVER:
		m.DataBits = int(v)
		if h.worker.mode.DataBits == m.DataBits {
			// return
		}
		info += " dataBits: "
		if v > 0 {
			info += fmt.Sprintf("%d", v)
		} else {
			info += "?"
		}
		log.Print(info, "\r\n")
		if h.client {
			h.worker.mode.DataBits = m.DataBits
			return
		}
		if v > 0 {
			h.worker.SetMode(&m)
		} else {
			h.meSet(c, h.worker.mode, e)
		}
		h.DataBits(c)
	case PARITY, PARITY + SERVER:
		info += " parity: "
		switch v {
		case 1:
			m.Parity = serial.NoParity
			info += "N"
		case 2:
			m.Parity = serial.OddParity
			info += "O"
		case 3:
			m.Parity = serial.EvenParity
			info += "E"
		case 4:
			m.Parity = serial.MarkParity
			info += "M"
		case 5:
			m.Parity = serial.SpaceParity
			info += "S"
		default:
			info += "?"
		}
		if h.worker.mode.Parity == m.Parity {
			// return
		}
		log.Print(info, "\r\n")
		if h.client {
			h.worker.mode.Parity = m.Parity
			return
		}
		if v > 0 {
			h.worker.SetMode(&m)
		} else {
			h.meSet(c, h.worker.mode, e)
		}
		h.Parity(c)
	case STOPSIZE, STOPSIZE + SERVER:
		info += " stopBits: "
		switch v {
		case 1:
			m.StopBits = serial.OneStopBit
			info += "1"
		case 2:
			m.StopBits = serial.TwoStopBits
			info += "2"
		case 3:
			m.StopBits = serial.OnePointFiveStopBits
			info += "1.5"
		default:
			info += "?"
		}
		if h.worker.mode.StopBits == m.StopBits {
			// return
		}
		log.Print(info, "\r\n")
		if h.client {
			h.worker.mode.StopBits = m.StopBits
			return
		}
		if v > 0 {
			h.worker.SetMode(&m)
		} else {
			h.meSet(c, h.worker.mode, e)
		}
		h.StopBits(c)
	case CONTROL, CONTROL + SERVER:
		info += " flowControl: N"
	}
}

func (h *Handler2217) setMode(c *telnet.Connection, mode *serial.Mode) {
	m, e := h.me(c)
	if !e {
		return
	}
	poll := false
	if mode == nil {
		mode = &h.worker.mode
		poll = true
	}

	ok := false
	if m.BaudRate != mode.BaudRate {
		ok = true
		defer func() { h.BaudRate(c) }()
	}
	if m.DataBits != mode.DataBits {
		ok = true
		defer func() { h.DataBits(c) }()
	}
	if m.Parity != mode.Parity {
		ok = true
		defer func() { h.Parity(c) }()
	}
	if m.StopBits != mode.StopBits {
		ok = true
		defer func() { h.StopBits(c) }()
	}
	if ok {
		if !poll {
			log.Printf("%s setMode %v to %s\r\n", c.LocalAddr(), *mode, c.RemoteAddr())
		}
		h.meSet(c, *mode, e)
	}
}
